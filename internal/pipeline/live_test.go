package pipeline

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danielkwan-dev/memento/internal/letterboxd"
	"github.com/danielkwan-dev/memento/internal/store"
)

// memStore is an in-memory Store implementation, so the whole pipeline can be
// exercised against the real Letterboxd without needing Postgres.
type memStore struct {
	mu      sync.Mutex
	details map[string]*letterboxd.FilmDetails
	stats   map[string]*letterboxd.FilmStats
	watch   []letterboxd.WatchEntry
	diary   []letterboxd.DiaryEntry
	phases  []store.JobPhase
}

func newMemStore() *memStore {
	return &memStore{
		details: map[string]*letterboxd.FilmDetails{},
		stats:   map[string]*letterboxd.FilmStats{},
	}
}

func (m *memStore) UpsertUser(context.Context, string) (int64, error) { return 1, nil }

func (m *memStore) ClassifySlugs(_ context.Context, slugs []string, _, _ time.Duration) (*store.StaleSplit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := &store.StaleSplit{}
	for _, s := range slugs {
		if _, ok := m.details[s]; ok {
			out.CacheHits++
			continue
		}
		out.NeedDetails = append(out.NeedDetails, s)
	}
	return out, nil
}

func (m *memStore) UpsertFilmDetails(_ context.Context, d *letterboxd.FilmDetails) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.details[d.Slug] = d
	return nil
}

func (m *memStore) UpsertFilmStats(_ context.Context, s *letterboxd.FilmStats) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats[s.Slug] = s
	return nil
}

func (m *memStore) EnsureFilmStubs(context.Context, []string, []string, []int32) error {
	return nil
}

func (m *memStore) ReplaceWatchEntries(_ context.Context, _ int64, e []letterboxd.WatchEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watch = e
	return nil
}

func (m *memStore) ReplaceDiaryEntries(_ context.Context, _ int64, e []letterboxd.DiaryEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diary = e
	return nil
}

func (m *memStore) MarkUserSynced(context.Context, int64) error { return nil }

func (m *memStore) UpdateProgress(_ context.Context, _ uuid.UUID, phase store.JobPhase, done, total, hits int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := len(m.phases); n == 0 || m.phases[n-1] != phase {
		m.phases = append(m.phases, phase)
	}
	return nil
}

// TestLivePipeline runs the real pipeline against the real Letterboxd, end to
// end, with an in-memory store. It is the check that the scraper, parsers and
// orchestration actually work together on live markup.
//
//	MEMENTO_LIVE=1 MEMENTO_LIVE_USER=<username> go test ./internal/pipeline/ -run TestLivePipeline -v
//
// Use a small profile: this makes real requests.
func TestLivePipeline(t *testing.T) {
	if os.Getenv("MEMENTO_LIVE") != "1" {
		t.Skip("set MEMENTO_LIVE=1 to run live network tests")
	}
	username := os.Getenv("MEMENTO_LIVE_USER")
	if username == "" {
		t.Skip("set MEMENTO_LIVE_USER to a Letterboxd username")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	fetcher, err := letterboxd.NewClient(letterboxd.DefaultConfig(), log)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	cfg := DefaultConfig()
	// Be gentle against the real site.
	cfg.IndexConcurrency = 2
	cfg.DetailConcurrency = 6

	mem := newMemStore()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	start := time.Now()
	res, err := New(fetcher, mem, cfg, log).Run(ctx, uuid.New(), username)
	if err != nil {
		t.Fatalf("live pipeline failed for %q: %v", username, err)
	}

	t.Logf("scraped %s in %s: watch=%d diary=%d fetched=%d stats=%d failures=%d",
		username, time.Since(start).Round(time.Second),
		res.WatchEntries, res.DiaryEntries, res.FilmsFetched,
		res.StatsFetched, res.FilmFailures)

	requests, retries, blocks, switches := fetcher.Snapshot()
	t.Logf("http: requests=%d retries=%d blocks=%d profile_switches=%d",
		requests, retries, blocks, switches)

	if res.WatchEntries == 0 {
		t.Error("no watch entries scraped")
	}

	// Spot-check that the stored films actually carry parsed metadata, rather
	// than the pipeline succeeding with empty rows.
	mem.mu.Lock()
	defer mem.mu.Unlock()

	var withGenres, withRuntime, withDirector, withStats int
	for _, d := range mem.details {
		if len(d.Genres) > 0 {
			withGenres++
		}
		if d.RuntimeMin > 0 {
			withRuntime++
		}
		if len(d.Directors) > 0 {
			withDirector++
		}
	}
	withStats = len(mem.stats)

	total := len(mem.details)
	if total == 0 {
		t.Fatal("no film details stored")
	}
	t.Logf("of %d films: genres=%d runtime=%d director=%d stats=%d",
		total, withGenres, withRuntime, withDirector, withStats)

	// These are the fields every chart depends on; a systemic parse failure would
	// show up as a low ratio here even though the job "succeeded".
	if float64(withGenres)/float64(total) < 0.8 {
		t.Errorf("only %d/%d films have genres; the genre parser may be broken",
			withGenres, total)
	}
	if float64(withRuntime)/float64(total) < 0.8 {
		t.Errorf("only %d/%d films have a runtime", withRuntime, total)
	}
	if float64(withDirector)/float64(total) < 0.8 {
		t.Errorf("only %d/%d films have a director", withDirector, total)
	}
	if float64(withStats)/float64(total) < 0.5 {
		t.Errorf("only %d/%d films have watch/like stats", withStats, total)
	}
}
