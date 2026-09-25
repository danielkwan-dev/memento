// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL string
	Port        int
	WorkerPort  int
	// WorkerHealthURL lets the API wake a scaled-to-zero worker machine before
	// enqueueing a job. Empty disables the wake-up probe.
	WorkerHealthURL string
	CORSOrigins     []string

	// IndexConcurrency bounds requests to a user's grid and diary pages. These
	// are NOT in Cloudflare's edge cache, so they must stay low.
	IndexConcurrency int
	// DetailConcurrency bounds requests to film detail pages, which ARE edge
	// cached and tolerate far more parallelism (measured: 12 films at 16-concurrent
	// in ~281ms, zero blocks).
	DetailConcurrency int
	// DetailIntervalMS paces detail requests, in milliseconds.
	DetailIntervalMS int
	// IndexIntervalMS paces the uncached grid/diary requests, in milliseconds.
	IndexIntervalMS int

	// DetailsTTL covers near-immutable metadata (genres, runtime, cast).
	DetailsTTL time.Duration
	// StatsTTL covers popularity counters, which drift continuously.
	StatsTTL time.Duration

	LogLevel string
}

func Load() (*Config, error) {
	c := &Config{
		DatabaseURL:       env("MEMENTO_DATABASE_URL", "postgres://memento:memento@localhost:5432/memento?sslmode=disable"),
		Port:              envInt("MEMENTO_PORT", 8080),
		WorkerPort:        envInt("MEMENTO_WORKER_PORT", 8081),
		WorkerHealthURL:   env("MEMENTO_WORKER_HEALTH_URL", ""),
		CORSOrigins:       envList("MEMENTO_CORS_ORIGINS", []string{"http://localhost:5173"}),
		IndexConcurrency:  envInt("MEMENTO_INDEX_CONCURRENCY", 3),
		DetailConcurrency: envInt("MEMENTO_DETAIL_CONCURRENCY", 12),
		DetailIntervalMS:  envInt("MEMENTO_DETAIL_INTERVAL_MS", 120),
		IndexIntervalMS:   envInt("MEMENTO_INDEX_INTERVAL_MS", 1500),
		DetailsTTL:        envDuration("MEMENTO_DETAILS_TTL", 90*24*time.Hour),
		StatsTTL:          envDuration("MEMENTO_STATS_TTL", 7*24*time.Hour),
		LogLevel:          env("MEMENTO_LOG_LEVEL", "info"),
	}

	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("MEMENTO_DATABASE_URL is required")
	}
	if c.IndexConcurrency < 1 || c.DetailConcurrency < 1 {
		return nil, fmt.Errorf("concurrency values must be >= 1")
	}
	return c, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}

func envList(key string, def []string) []string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
