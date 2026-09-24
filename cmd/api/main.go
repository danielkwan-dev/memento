// Command api serves the HTTP API.
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

	"github.com/danielkwan-dev/memento/internal/api"
	"github.com/danielkwan-dev/memento/internal/config"
	"github.com/danielkwan-dev/memento/internal/store"
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

	// Signal-aware root context: SIGTERM starts a graceful shutdown, which is
	// how container platforms ask a process to stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStoreWithRetry(ctx, cfg.DatabaseURL, log)
	if err != nil {
		return err
	}
	defer st.Close()

	// Migrating at startup means the process cannot serve against an old schema,
	// and there is no separate deploy step to forget.
	migrateCtx, cancelMigrate := context.WithTimeout(ctx, 60*time.Second)
	defer cancelMigrate()
	if err := st.Migrate(migrateCtx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Info("migrations applied")

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: api.NewServer(cfg, st, log).Routes(),
		// No WriteTimeout: SSE responses are long-lived by design and a write
		// deadline would sever them mid-stream.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("api listening", "port", cfg.Port,
			"worker_wake_url", cfg.WorkerHealthURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// openStoreWithRetry tolerates the database not being ready yet, which happens
// routinely when the whole stack starts at once.
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
	// JSON so hosted log aggregators can parse it.
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
