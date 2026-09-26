// Package api exposes the HTTP surface: sync, job progress over SSE, and stats.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"

	"github.com/danielkwan-dev/memento/internal/config"
	"github.com/danielkwan-dev/memento/internal/letterboxd"
	"github.com/danielkwan-dev/memento/internal/stats"
	"github.com/danielkwan-dev/memento/internal/store"
)

// usernameRe matches Letterboxd's own username rules. Validating up front keeps
// obvious junk from ever reaching the scraper.
var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_]{2,32}$`)

type Server struct {
	cfg   *config.Config
	store *store.Store
	stats *stats.Service
	log   *slog.Logger

	// waker probes the worker's health endpoint so a scaled-to-zero worker
	// machine starts booting the moment a job is enqueued.
	waker *Waker
}

func NewServer(cfg *config.Config, st *store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:   cfg,
		store: st,
		stats: stats.New(st.Pool()),
		log:   log,
		waker: NewWaker(cfg.WorkerHealthURL, log),
	}
}

func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(s.logRequests)
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: s.cfg.CORSOrigins,
		AllowedMethods: []string{"GET", "POST", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{"Accept", "Content-Type", "Last-Event-ID"},
		MaxAge:         300,
	}))

	// Liveness and readiness. The frontend also calls /healthz on page load to
	// wake a scaled-to-zero machine before the user submits anything.
	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleReady)

	r.Route("/api/v1", func(r chi.Router) {
		// Scraping is expensive and hits a third party, so it is rate limited
		// per client well below the general read limit.
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(15 * time.Second))
			r.Post("/sync", s.handleSync)
		})

		r.Post("/import", s.handleImport)

		r.Get("/jobs/{id}", s.handleJob)
		// SSE must not sit behind a request timeout: it is a long-lived stream.
		r.Get("/jobs/{id}/events", s.handleJobEvents)

		r.Get("/users/{username}/stats", s.handleAllStats)
		r.Get("/users/{username}/stats/{category}", s.handleCategoryStats)
		r.Delete("/users/{username}", s.handleDeleteUser)
	})

	// The built frontend is served by this same process in the container image, so
	// there is one deploy, one origin and no CORS to configure. Registered last, as
	// a catch-all beneath the API routes; absent in local development, where Vite
	// serves the app on its own port.
	if StaticFS != nil {
		r.Handle("/*", staticHandler(StaticFS))
	}

	return r
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		// SSE streams last minutes; logging them at info would be noise.
		level := slog.LevelInfo
		if strings.HasSuffix(r.URL.Path, "/events") {
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "http",
			"method", r.Method, "path", r.URL.Path,
			"status", ww.Status(), "bytes", ww.BytesWritten(),
			"duration", time.Since(start).Round(time.Millisecond),
			"request_id", middleware.GetReqID(r.Context()))
	})
}

// --- health ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Touching the worker here means the frontend's page-load probe warms BOTH
	// machines: the API answering this request, and the worker that will run the
	// job. Without it the API could be warm while the worker is still cold, and
	// a job would sit queued with nothing to claim it.
	s.waker.Touch()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC(),
	})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// --- sync ---

type syncRequest struct {
	Username string `json:"username"`
}

type syncResponse struct {
	Job          *store.Job `json:"job"`
	AlreadyInFli bool       `json:"already_in_flight,omitempty"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	var req syncRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	username := strings.ToLower(strings.TrimSpace(req.Username))
	if !usernameRe.MatchString(username) {
		writeError(w, http.StatusBadRequest,
			"username must be 2-32 characters of letters, digits or underscores")
		return
	}

	// Start waking the worker before enqueueing, so its boot overlaps the
	// enqueue round trip instead of following it.
	s.waker.Touch()

	job, err := s.store.EnqueueJob(r.Context(), username, store.KindScrape)
	if err != nil {
		if errors.Is(err, store.ErrJobInFlight) {
			// Idempotent: hand back the running job rather than starting a
			// second scrape of the same profile.
			existing, lookupErr := s.store.ActiveJobFor(r.Context(), username)
			if lookupErr != nil || existing == nil {
				writeError(w, http.StatusConflict, "a sync for this user is already running")
				return
			}
			writeJSON(w, http.StatusAccepted, syncResponse{Job: existing, AlreadyInFli: true})
			return
		}
		s.log.Error("enqueue failed", "username", username, "err", err)
		writeError(w, http.StatusInternalServerError, "could not queue the sync")
		return
	}

	resp := syncResponse{Job: job}
	if t, ok, err := s.store.LastSyncedAt(r.Context(), username); err == nil && ok {
		resp.LastSyncedAt = &t
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// maxExportSize bounds an uploaded archive. A Letterboxd export of a very large
// profile is a few hundred KB of CSV, so this is generous while still refusing
// anything that would be a problem to hold in memory or in a row.
const maxExportSize = 32 << 20 // 32 MiB

// handleImport ingests a Letterboxd data export instead of scraping.
//
// This path skips the paginated user pages entirely -- the most aggressively
// blocked requests in a scrape -- so it keeps working when scraping does not.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "expected a multipart form with an export file")
		return
	}

	username := strings.ToLower(strings.TrimSpace(r.FormValue("username")))
	if !usernameRe.MatchString(username) {
		writeError(w, http.StatusBadRequest,
			"username must be 2-32 characters of letters, digits or underscores")
		return
	}

	file, header, err := r.FormFile("export")
	if err != nil {
		writeError(w, http.StatusBadRequest, "no export file was uploaded")
		return
	}
	defer file.Close()

	if header.Size > maxExportSize {
		writeError(w, http.StatusRequestEntityTooLarge, "that export is too large")
		return
	}

	// Read through a limit reader as well: Size is client-supplied.
	payload, err := io.ReadAll(io.LimitReader(file, maxExportSize+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the uploaded file")
		return
	}
	if len(payload) > maxExportSize {
		writeError(w, http.StatusRequestEntityTooLarge, "that export is too large")
		return
	}

	// Validate before enqueueing, so a bad upload fails immediately with a useful
	// message instead of becoming a failed background job.
	if _, err := letterboxd.ParseExportZip(payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.waker.Touch()

	job, err := s.store.EnqueueImportJob(r.Context(), username, payload)
	if err != nil {
		if errors.Is(err, store.ErrJobInFlight) {
			existing, lookupErr := s.store.ActiveJobFor(r.Context(), username)
			if lookupErr != nil || existing == nil {
				writeError(w, http.StatusConflict, "a sync for this user is already running")
				return
			}
			writeJSON(w, http.StatusAccepted, syncResponse{Job: existing, AlreadyInFli: true})
			return
		}
		s.log.Error("enqueue import failed", "username", username, "err", err)
		writeError(w, http.StatusInternalServerError, "could not queue the import")
		return
	}
	writeJSON(w, http.StatusAccepted, syncResponse{Job: job})
}

// --- jobs ---

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid job id")
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
	writeJSON(w, http.StatusOK, job)
}

// --- stats ---

func (s *Server) handleAllStats(w http.ResponseWriter, r *http.Request) {
	username := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "username")))
	userID, ok, err := s.store.UserID(r.Context(), username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the user")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no data for this user yet; run a sync first")
		return
	}

	out := make(map[string]any, len(stats.AllCategories))
	for _, cat := range stats.AllCategories {
		payload, err := s.stats.Compute(r.Context(), userID, cat)
		if err != nil {
			s.log.Error("stats failed", "category", cat, "username", username, "err", err)
			writeError(w, http.StatusInternalServerError, "could not compute stats")
			return
		}
		out[string(cat)] = payload
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCategoryStats(w http.ResponseWriter, r *http.Request) {
	username := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "username")))
	cat, ok := stats.ParseCategory(chi.URLParam(r, "category"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown stats category")
		return
	}
	userID, found, err := s.store.UserID(r.Context(), username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load the user")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no data for this user yet; run a sync first")
		return
	}
	payload, err := s.stats.Compute(r.Context(), userID, cat)
	if err != nil {
		s.log.Error("stats failed", "category", cat, "err", err)
		writeError(w, http.StatusInternalServerError, "could not compute stats")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	username := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "username")))
	// Removes the user's own entries and cached stats. The global films table is
	// untouched: it holds only public facts about films, nothing personal.
	deleted, err := s.store.DeleteUser(r.Context(), username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete the user")
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "no data for this user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so this can only be logged.
		slog.Default().Error("encode response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
