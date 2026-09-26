// Package store holds all database access. Nothing above it writes SQL.
package store

import (
	"context"
	"embed"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// envInt keeps store.Open self-contained; the config package depends on nothing
// here, and this is the only knob the store itself needs.
func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	// Free-tier Postgres allows few connections, and this app runs two processes
	// (api + worker) against one database. Each live SSE stream also holds a
	// dedicated connection for LISTEN, so the ceiling is reached faster than the
	// query load suggests.
	//
	// The connection must be DIRECT, not pooled: PgBouncer in transaction mode
	// (what Neon and most managed poolers run) does not support LISTEN/NOTIFY,
	// and the SSE progress stream would silently never fire.
	cfg.MaxConns = int32(envInt("MEMENTO_DB_MAX_CONNS", 5))
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping backs the readiness probe.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies every embedded up-migration that has not run yet. Keeping
// this in-process means a deploy cannot start serving against an old schema,
// and there is no separate migration step to forget.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}

	var versions []string
	files := make(map[string]string, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		version := strings.TrimSuffix(name, ".up.sql")
		versions = append(versions, version)
		files[version] = name
	}
	sort.Strings(versions)

	for _, version := range versions {
		var applied bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`,
			version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if applied {
			continue
		}

		body, err := migrationFS.ReadFile("migrations/" + files[version])
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}

		// One transaction per migration: a failure leaves no partial schema.
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %s: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", version, err)
		}
	}
	return nil
}
