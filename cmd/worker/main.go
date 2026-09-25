// Command worker claims scrape jobs and runs the pipeline.
//
// It also serves a tiny health endpoint. That endpoint is what makes the worker
// wakeable: on free-tier hosting an idle machine is scaled to zero, and an HTTP
// request is what boots it. The API pings it when a job is enqueued.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/danielkwan-dev/memento/internal/config"
	"github.com/danielkwan-dev/memento/internal/letterboxd"
	"github.com/danielkwan-dev/memento/internal/pipeline"
	"github.com/danielkwan-dev/memento/internal/store"
)

const (
	// idlePoll is the fallback when no NOTIFY arrives. LISTEN handles the normal
	// case; this covers a notification sent while the worker was still booting.
	idlePoll = 5 * time.Second
	// jobTimeout bounds one scrape. A 2000-film cold cache is slow but not
	// unbounded, and a stuck job must not hold the queue forever.
	jobTimeout = 25 * time.Minute
	// staleAfter is how long a running job may go without a progress update
	// before it is presumed dead and requeued.
	staleAfter  = 5 * time.Minute
	maxAttempts = 3
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	workerID := os.Getenv("FLY_MACHINE_ID")
	if workerID == "" {
		host, _ := os.Hostname()
		workerID = fmt.Sprintf("%s-%s", host, uuid.New().String()[:8])
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStoreWithRetry(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer st.Close()

	// The API also migrates; both are idempotent and guarded per version, so
	// whichever starts first wins and the other is a no-op.
	migrateCtx, cancelMigrate := context.WithTimeout(ctx, 60*time.Second)
	defer cancelMigrate()
	if err := st.Migrate(migrateCtx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	fetcher, err := letterboxd.NewClient(letterboxd.DefaultConfig(), log)
	if err != nil {
		return fmt.Errorf("build fetcher: %w", err)
	}

	pipeCfg := pipeline.DefaultConfig()
	pipeCfg.IndexConcurrency = cfg.IndexConcurrency
	pipeCfg.DetailConcurrency = cfg.DetailConcurrency
	pipeCfg.IndexMinInterval = time.Duration(cfg.IndexIntervalMS) * time.Millisecond
	pipeCfg.DetailMinInterval = time.Duration(cfg.DetailIntervalMS) * time.Millisecond
	pipeCfg.DetailsTTL = cfg.DetailsTTL
	pipeCfg.StatsTTL = cfg.StatsTTL
	pipe := pipeline.New(fetcher, st, pipeCfg, log)

	// Health server: its only job is to exist so an HTTP probe can wake this
	// machine from zero.
	health := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.WorkerPort),
		Handler:           healthHandler(st),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("worker health listening", "port", cfg.WorkerPort)
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server failed", "err", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = health.Shutdown(shutdownCtx)
	}()

	go reapLoop(ctx, st, log)

	log.Info("worker started", "worker_id", workerID,
		"index_concurrency", cfg.IndexConcurrency,
		"detail_concurrency", cfg.DetailConcurrency)

	return workLoop(ctx, st, pipe, workerID, log)
}

// workLoop drains the queue, then waits for a NOTIFY or the idle timer.
func workLoop(ctx context.Context, st *store.Store, pipe *pipeline.Pipeline, workerID string, log *slog.Logger) error {
	// Subscribe before the first drain so a job enqueued during startup is not
	// missed between the drain and the subscription.
	queued, err := st.Listen(ctx, store.QueueChannel)
	if err != nil {
		log.Warn("queue LISTEN unavailable, falling back to polling", "err", err)
		queued = nil
	}

	timer := time.NewTimer(idlePoll)
	defer timer.Stop()

	for {
		// Drain everything available before sleeping again.
		for {
			if ctx.Err() != nil {
				return nil
			}
			job, err := st.ClaimJob(ctx, workerID)
			if err != nil {
				log.Error("claim failed", "err", err)
				break
			}
			if job == nil {
				break // queue empty
			}
			runJob(ctx, st, pipe, job, log)
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(idlePoll)

		select {
		case <-ctx.Done():
			return nil
		case <-queued:
			// A job was just enqueued; loop straight back and claim it.
		case <-timer.C:
		}
	}
}

func runJob(ctx context.Context, st *store.Store, pipe *pipeline.Pipeline, job *store.Job, log *slog.Logger) {
	log = log.With("job", job.ID, "username", job.Username, "attempt", job.Attempts)
	log.Info("job started")
	start := time.Now()

	jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	var (
		res *pipeline.Result
		err error
	)
	switch job.Kind {
	case store.KindImport:
		// The uploaded archive travels in the jobs row; read it here rather than
		// on the Job struct so progress notifications never carry the payload.
		var payload []byte
		payload, err = st.JobPayload(jobCtx, job.ID)
		if err == nil && len(payload) == 0 {
			err = errors.New("import job has no payload (it may have been retried after completion)")
		}
		if err == nil {
			res, err = pipe.RunImport(jobCtx, job.ID, job.Username, payload)
		}
	default:
		res, err = pipe.Run(jobCtx, job.ID, job.Username)
	}

	if err != nil {
		// Shutdown is not a job failure: leave the job running so the reaper
		// requeues it for the next worker rather than reporting failure.
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			log.Warn("job interrupted by shutdown; will be requeued")
			return
		}
		log.Error("job failed", "err", err, "duration", time.Since(start).Round(time.Second))
		if ferr := st.FinishJob(context.WithoutCancel(jobCtx), job.ID, err.Error()); ferr != nil {
			log.Error("could not mark job failed", "err", ferr)
		}
		return
	}

	if ferr := st.FinishJob(context.WithoutCancel(jobCtx), job.ID, ""); ferr != nil {
		log.Error("could not mark job succeeded", "err", ferr)
	}
	// The archive has served its purpose; a finished job should not keep holding
	// the upload.
	if job.Kind == store.KindImport {
		if cerr := st.ClearJobPayload(context.WithoutCancel(jobCtx), job.ID); cerr != nil {
			log.Warn("could not clear import payload", "err", cerr)
		}
	}
	log.Info("job succeeded",
		"watch_entries", res.WatchEntries, "diary_entries", res.DiaryEntries,
		"films_fetched", res.FilmsFetched, "cache_hits", res.CacheHits,
		"film_failures", res.FilmFailures,
		"duration", time.Since(start).Round(time.Second))
}

// reapLoop requeues jobs whose worker died, so a crash or a scaled-to-zero
// machine never leaves a user's job stuck.
func reapLoop(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			requeued, failed, err := st.ReapStaleJobs(ctx, staleAfter, maxAttempts)
			if err != nil {
				log.Warn("reap failed", "err", err)
				continue
			}
			if requeued > 0 || failed > 0 {
				log.Info("reaped stale jobs", "requeued", requeued, "failed", failed)
			}
		}
	}
}

func healthHandler(st *store.Store) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","role":"worker"}`))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"database unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	return mux
}

func openStoreWithRetry(ctx context.Context, dsn string, log *slog.Logger) (*store.Store, error) {
	const attempts = 10
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, err := store.Open(ctx, dsn)
		if err == nil {
			return st, nil
		}
		lastErr = err
		wait := time.Duration(i+1) * time.Second
		log.Warn("database not ready, retrying", "attempt", i+1, "wait", wait, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, fmt.Errorf("connect to database after %d attempts: %w", attempts, lastErr)
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
