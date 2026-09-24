package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Waker nudges the worker process awake.
//
// On free-tier hosting (Fly.io, Railway and similar) an idle machine is scaled
// to zero and only boots when traffic arrives. The API machine waking on an
// incoming request does NOT wake the worker machine, so without this a job would
// be enqueued and then sit in 'queued' with nothing running to claim it. An
// HTTP probe to the worker's health endpoint is what starts its boot.
//
// Touch is fire-and-forget and heavily throttled: the goal is to overlap the
// worker's boot with work the user is already waiting through (reading the page,
// the enqueue round trip), not to add latency to the request that triggered it.
type Waker struct {
	url    string
	log    *slog.Logger
	client *http.Client

	mu       sync.Mutex
	lastPing time.Time
	inFlight bool
}

// wakeThrottle is the minimum gap between probes. A cold boot takes a few
// seconds, so probing more often than this cannot help and only adds noise.
const wakeThrottle = 10 * time.Second

func NewWaker(url string, log *slog.Logger) *Waker {
	if log == nil {
		log = slog.Default()
	}
	return &Waker{
		url: url,
		log: log,
		// Generous timeout: a cold machine legitimately takes seconds to answer,
		// and this runs in the background where nothing is waiting on it.
		client: &http.Client{Timeout: 25 * time.Second},
	}
}

// Enabled reports whether a worker URL was configured. Local development and
// single-process deployments leave it empty.
func (w *Waker) Enabled() bool { return w != nil && w.url != "" }

// Touch asks the worker to wake, if it has not been asked recently. It returns
// immediately; the probe runs in the background.
func (w *Waker) Touch() {
	if !w.Enabled() {
		return
	}

	w.mu.Lock()
	if w.inFlight || time.Since(w.lastPing) < wakeThrottle {
		w.mu.Unlock()
		return
	}
	w.inFlight = true
	w.lastPing = time.Now()
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			w.inFlight = false
			w.mu.Unlock()
		}()

		// Detached from the request context on purpose: the probe must outlive
		// the HTTP request that triggered it, or it would be cancelled the
		// instant that response is written.
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.url, nil)
		if err != nil {
			w.log.Warn("build wake request", "url", w.url, "err", err)
			return
		}

		start := time.Now()
		resp, err := w.client.Do(req)
		if err != nil {
			// Expected while the machine is still booting. The worker also polls
			// the queue on a timer, so a failed wake delays a job rather than
			// losing it.
			w.log.Debug("worker wake probe failed",
				"url", w.url, "err", err, "elapsed", time.Since(start))
			return
		}
		defer resp.Body.Close()
		w.log.Debug("worker wake probe ok",
			"status", resp.StatusCode, "elapsed", time.Since(start).Round(time.Millisecond))
	}()
}
