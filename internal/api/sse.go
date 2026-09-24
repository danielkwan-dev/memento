package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/danielkwan-dev/memento/internal/store"
)

// heartbeatInterval keeps idle SSE connections alive. Proxies and load balancers
// commonly close a connection that has been silent for 30-60 seconds, and during
// a cold start or a long hydrate phase there can be real gaps between ticks.
const heartbeatInterval = 15 * time.Second

// handleJobEvents streams a job's progress as Server-Sent Events.
//
// SSE rather than WebSockets: progress is strictly server-to-client, SSE is
// plain HTTP so it needs no upgrade negotiation and traverses any proxy, and
// browsers reconnect automatically on a dropped connection.
func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	job, err := s.store.JobByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the job")
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable proxy buffering, which would otherwise hold events until the
	// response completes and defeat the whole point of streaming.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()

	// Subscribe BEFORE sending the current state. Doing it the other way round
	// leaves a window in which a tick fires between the snapshot and the
	// subscription, and that update would be lost.
	notifications, err := s.store.Listen(ctx, store.JobChannel(id))
	if err != nil {
		s.log.Error("listen failed", "job", id, "err", err)
		writeSSE(w, flusher, "error", map[string]string{"error": "could not subscribe to progress"})
		return
	}

	// Send the current state immediately so a client that connects mid-job (or
	// reconnects) renders the right thing without waiting for the next tick.
	writeSSE(w, flusher, "progress", store.JobProgress{
		JobID:      job.ID,
		Status:     job.Status,
		Phase:      job.Phase,
		FilmsTotal: job.FilmsTotal,
		FilmsDone:  job.FilmsDone,
		CacheHits:  job.CacheHits,
		Error:      job.Error,
	})

	// The job may already have finished before this connection opened.
	if job.Terminal() {
		writeSSE(w, flusher, terminalEvent(job.Status), finalPayload(job))
		return
	}

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	// Safety net: a job whose worker dies without publishing a terminal event
	// would otherwise hold this stream open forever. Re-reading state
	// periodically lets the stream close on its own.
	poll := time.NewTicker(20 * time.Second)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case payload, ok := <-notifications:
			if !ok {
				return // listener connection closed
			}
			var p store.JobProgress
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				s.log.Warn("bad progress payload", "job", id, "err", err)
				continue
			}
			writeSSE(w, flusher, "progress", p)
			if p.Status == store.StatusSucceeded || p.Status == store.StatusFailed ||
				p.Status == store.StatusCancelled {
				writeSSE(w, flusher, terminalEvent(p.Status), p)
				return
			}

		case <-heartbeat.C:
			// A comment line keeps the connection open without being delivered
			// to the client's message handler.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()

		case <-poll.C:
			current, err := s.store.JobByID(ctx, id)
			if err != nil || current == nil {
				continue
			}
			if current.Terminal() {
				writeSSE(w, flusher, terminalEvent(current.Status), finalPayload(current))
				return
			}
		}
	}
}

func terminalEvent(status store.JobStatus) string {
	if status == store.StatusSucceeded {
		return "complete"
	}
	return "error"
}

func finalPayload(j *store.Job) store.JobProgress {
	return store.JobProgress{
		JobID:      j.ID,
		Status:     j.Status,
		Phase:      j.Phase,
		FilmsTotal: j.FilmsTotal,
		FilmsDone:  j.FilmsDone,
		CacheHits:  j.CacheHits,
		Error:      j.Error,
	}
}

func writeSSE(w http.ResponseWriter, f http.Flusher, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	f.Flush()
}
