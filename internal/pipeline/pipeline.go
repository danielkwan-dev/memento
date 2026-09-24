// Package pipeline orchestrates a scrape: index a user's pages, hydrate only
// the films the cache is missing, then persist everything atomically.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/danielkwan-dev/memento/internal/letterboxd"
	"github.com/danielkwan-dev/memento/internal/store"
)

// Store is the persistence surface the pipeline needs. Narrowing it to an
// interface keeps the pipeline testable without a database.
type Store interface {
	UpsertUser(ctx context.Context, username string) (int64, error)
	ClassifySlugs(ctx context.Context, slugs []string, detailsTTL, statsTTL time.Duration) (*store.StaleSplit, error)
	UpsertFilmDetails(ctx context.Context, d *letterboxd.FilmDetails) error
	UpsertFilmStats(ctx context.Context, s *letterboxd.FilmStats) error
	EnsureFilmStubs(ctx context.Context, slugs, titles []string, years []int32) error
	ReplaceWatchEntries(ctx context.Context, userID int64, entries []letterboxd.WatchEntry) error
	ReplaceDiaryEntries(ctx context.Context, userID int64, entries []letterboxd.DiaryEntry) error
	MarkUserSynced(ctx context.Context, userID int64) error
	UpdateProgress(ctx context.Context, id uuid.UUID, phase store.JobPhase, done, total, cacheHits int) error
}

type Config struct {
	// IndexConcurrency bounds requests to the user's own grid and diary pages.
	// These are not in Cloudflare's edge cache, so this stays small.
	IndexConcurrency int
	// DetailConcurrency bounds requests to film pages, which ARE edge cached and
	// therefore tolerate much more parallelism.
	DetailConcurrency int
	DetailsTTL        time.Duration
	StatsTTL          time.Duration
	// MaxFilmFailureRatio is the share of per-film fetch failures tolerated
	// before the whole job is considered failed. Individual films go missing for
	// mundane reasons; a large fraction failing means something systemic.
	MaxFilmFailureRatio float64
}

func DefaultConfig() Config {
	return Config{
		IndexConcurrency:    3,
		DetailConcurrency:   16,
		DetailsTTL:          90 * 24 * time.Hour,
		StatsTTL:            7 * 24 * time.Hour,
		MaxFilmFailureRatio: 0.2,
	}
}

type Pipeline struct {
	fetch letterboxd.Fetcher
	store Store
	cfg   Config
	log   *slog.Logger
}

func New(f letterboxd.Fetcher, s Store, cfg Config, log *slog.Logger) *Pipeline {
	if log == nil {
		log = slog.Default()
	}
	return &Pipeline{fetch: f, store: s, cfg: cfg, log: log}
}

// Result summarises what a run did, for logging and for the job record.
type Result struct {
	UserID       int64
	WatchEntries int
	DiaryEntries int
	FilmsFetched int
	StatsFetched int
	CacheHits    int
	FilmFailures int
}

// Run executes the full pipeline for a username.
func (p *Pipeline) Run(ctx context.Context, jobID uuid.UUID, username string) (*Result, error) {
	res := &Result{}

	// --- Phase: resolve ---
	if err := p.store.UpdateProgress(ctx, jobID, store.PhaseResolve, 0, 0, 0); err != nil {
		return nil, err
	}
	userID, err := p.store.UpsertUser(ctx, username)
	if err != nil {
		return nil, err
	}
	res.UserID = userID

	// --- Phase: index (grid + diary) ---
	if err := p.store.UpdateProgress(ctx, jobID, store.PhaseIndex, 0, 0, 0); err != nil {
		return nil, err
	}
	watch, diary, err := p.index(ctx, username)
	if err != nil {
		return nil, err
	}
	res.WatchEntries = len(watch)
	res.DiaryEntries = len(diary)

	if len(watch) == 0 && len(diary) == 0 {
		return nil, fmt.Errorf("no films found for %q: the profile may be empty or private", username)
	}

	// Stubs first so watch/diary rows have a valid films FK even before their
	// details land.
	if err := p.ensureStubs(ctx, watch, diary); err != nil {
		return nil, err
	}

	// --- Phase: hydrate (the cache-miss diff) ---
	slugs := uniqueSlugs(watch, diary)
	split, err := p.store.ClassifySlugs(ctx, slugs, p.cfg.DetailsTTL, p.cfg.StatsTTL)
	if err != nil {
		return nil, err
	}
	res.CacheHits = split.CacheHits

	total := len(split.NeedDetails) + len(split.NeedStats)
	p.log.Info("hydrate plan",
		"username", username, "films", len(slugs),
		"need_details", len(split.NeedDetails), "need_stats", len(split.NeedStats),
		"cache_hits", split.CacheHits)

	if err := p.store.UpdateProgress(ctx, jobID, store.PhaseHydrate, 0, total, split.CacheHits); err != nil {
		return nil, err
	}

	fetched, statsFetched, failures, err := p.hydrate(ctx, jobID, split, total)
	if err != nil {
		return nil, err
	}
	res.FilmsFetched = fetched
	res.StatsFetched = statsFetched
	res.FilmFailures = failures

	// A handful of missing films is normal; a large share means the parser or
	// the network is broken, and silently storing a gutted profile would be
	// worse than failing loudly.
	if total > 0 {
		if ratio := float64(failures) / float64(total); ratio > p.cfg.MaxFilmFailureRatio {
			return nil, fmt.Errorf("hydrate failed for %d of %d films (%.0f%%), above the %.0f%% threshold",
				failures, total, ratio*100, p.cfg.MaxFilmFailureRatio*100)
		}
	}

	// --- Phase: persist ---
	if err := p.store.UpdateProgress(ctx, jobID, store.PhasePersist, total, total, split.CacheHits); err != nil {
		return nil, err
	}
	if err := p.store.ReplaceWatchEntries(ctx, userID, watch); err != nil {
		return nil, err
	}
	if err := p.store.ReplaceDiaryEntries(ctx, userID, diary); err != nil {
		return nil, err
	}
	if err := p.store.MarkUserSynced(ctx, userID); err != nil {
		return nil, err
	}

	p.log.Info("pipeline complete", "username", username,
		"watch", res.WatchEntries, "diary", res.DiaryEntries,
		"fetched", res.FilmsFetched, "cache_hits", res.CacheHits,
		"failures", res.FilmFailures)
	return res, nil
}

// index scrapes the films grid and the diary concurrently. Page 1 of each is
// fetched first to learn the page count, then the rest fan out under a small
// semaphore, since these pages are not edge cached.
func (p *Pipeline) index(ctx context.Context, username string) ([]letterboxd.WatchEntry, []letterboxd.DiaryEntry, error) {
	var (
		watch []letterboxd.WatchEntry
		diary []letterboxd.DiaryEntry
	)
	sem := semaphore.NewWeighted(int64(p.cfg.IndexConcurrency))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		var err error
		watch, err = p.indexGrid(gctx, username, sem)
		return err
	})
	g.Go(func() error {
		var err error
		diary, err = p.indexDiary(gctx, username, sem)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	return watch, diary, nil
}

func (p *Pipeline) indexGrid(ctx context.Context, username string, sem *semaphore.Weighted) ([]letterboxd.WatchEntry, error) {
	first, err := p.getPage(ctx, sem, fmt.Sprintf("%s/%s/films/", letterboxd.BaseURL, username))
	if err != nil {
		return nil, fmt.Errorf("fetch films grid for %q: %w", username, err)
	}
	entries, err := letterboxd.ParseFilmsGrid(first)
	if err != nil {
		return nil, err
	}
	lastPage, err := letterboxd.LastPageNumber(first)
	if err != nil {
		return nil, err
	}

	if lastPage > 1 {
		var mu sync.Mutex
		g, gctx := errgroup.WithContext(ctx)
		for page := 2; page <= lastPage; page++ {
			page := page
			g.Go(func() error {
				url := fmt.Sprintf("%s/%s/films/page/%d/", letterboxd.BaseURL, username, page)
				body, err := p.getPage(gctx, sem, url)
				if err != nil {
					return fmt.Errorf("films grid page %d: %w", page, err)
				}
				got, err := letterboxd.ParseFilmsGrid(body)
				if err != nil {
					return err
				}
				mu.Lock()
				entries = append(entries, got...)
				mu.Unlock()
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
	}
	return dedupeWatch(entries), nil
}

func (p *Pipeline) indexDiary(ctx context.Context, username string, sem *semaphore.Weighted) ([]letterboxd.DiaryEntry, error) {
	first, err := p.getPage(ctx, sem, fmt.Sprintf("%s/%s/films/diary/", letterboxd.BaseURL, username))
	if err != nil {
		// A missing diary is not fatal: plenty of users log films without dating
		// them, and every non-temporal chart still works.
		if errors.Is(err, letterboxd.ErrNotFound) {
			p.log.Warn("no diary found", "username", username)
			return nil, nil
		}
		return nil, fmt.Errorf("fetch diary for %q: %w", username, err)
	}
	entries, err := letterboxd.ParseDiaryPage(first)
	if err != nil {
		return nil, err
	}
	lastPage, err := letterboxd.LastPageNumber(first)
	if err != nil {
		return nil, err
	}

	if lastPage > 1 {
		var mu sync.Mutex
		g, gctx := errgroup.WithContext(ctx)
		for page := 2; page <= lastPage; page++ {
			page := page
			g.Go(func() error {
				url := fmt.Sprintf("%s/%s/films/diary/page/%d/", letterboxd.BaseURL, username, page)
				body, err := p.getPage(gctx, sem, url)
				if err != nil {
					return fmt.Errorf("diary page %d: %w", page, err)
				}
				got, err := letterboxd.ParseDiaryPage(body)
				if err != nil {
					return err
				}
				mu.Lock()
				entries = append(entries, got...)
				mu.Unlock()
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// hydrate fetches the missing film details and stats under the wider
// concurrency limit, reporting progress as it goes.
//
// Per-film failures are counted rather than propagated: one deleted film must
// not abandon a 2000-film scrape. Systemic failure is caught by the ratio check
// in Run.
func (p *Pipeline) hydrate(ctx context.Context, jobID uuid.UUID, split *store.StaleSplit, total int) (details, stats, failures int, err error) {
	sem := semaphore.NewWeighted(int64(p.cfg.DetailConcurrency))
	var (
		doneCount    atomic.Int64
		detailCount  atomic.Int64
		statsCount   atomic.Int64
		failureCount atomic.Int64
	)

	// Progress is reported on a ticker rather than per film: a 2000-film scrape
	// would otherwise issue 2000 UPDATE+NOTIFY round trips, and the UI cannot
	// use that resolution anyway.
	progressCtx, stopProgress := context.WithCancel(ctx)
	defer stopProgress()
	go func() {
		t := time.NewTicker(750 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-progressCtx.Done():
				return
			case <-t.C:
				_ = p.store.UpdateProgress(progressCtx, jobID, store.PhaseHydrate,
					int(doneCount.Load()), total, split.CacheHits)
			}
		}
	}()

	g, gctx := errgroup.WithContext(ctx)

	for _, slug := range split.NeedDetails {
		slug := slug
		g.Go(func() error {
			if err := sem.Acquire(gctx, 1); err != nil {
				return err
			}
			defer sem.Release(1)

			if err := p.fetchFilm(gctx, slug); err != nil {
				// Cancellation is the caller's decision, not a film failure.
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				p.log.Warn("film fetch failed", "slug", slug, "err", err)
				failureCount.Add(1)
			} else {
				detailCount.Add(1)
			}
			doneCount.Add(1)
			return nil
		})
	}

	for _, slug := range split.NeedStats {
		slug := slug
		g.Go(func() error {
			if err := sem.Acquire(gctx, 1); err != nil {
				return err
			}
			defer sem.Release(1)

			if err := p.fetchStats(gctx, slug); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				p.log.Warn("stats fetch failed", "slug", slug, "err", err)
				failureCount.Add(1)
			} else {
				statsCount.Add(1)
			}
			doneCount.Add(1)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return 0, 0, 0, err
	}
	stopProgress()

	return int(detailCount.Load()), int(statsCount.Load()), int(failureCount.Load()), nil
}

// fetchFilm pulls a film's detail page and its stats fragment, then stores both.
func (p *Pipeline) fetchFilm(ctx context.Context, slug string) error {
	body, err := p.fetch.Get(ctx, fmt.Sprintf("%s/film/%s/", letterboxd.BaseURL, slug))
	if err != nil {
		return err
	}
	details, err := letterboxd.ParseFilmDetails(slug, body)
	if err != nil {
		return err
	}
	if err := p.store.UpsertFilmDetails(ctx, details); err != nil {
		return err
	}
	// Stats live on a separate endpoint; failing to get them must not discard
	// the details we just stored.
	if err := p.fetchStats(ctx, slug); err != nil {
		p.log.Debug("stats unavailable", "slug", slug, "err", err)
	}
	return nil
}

func (p *Pipeline) fetchStats(ctx context.Context, slug string) error {
	body, err := p.fetch.Get(ctx, fmt.Sprintf("%s/csi/film/%s/stats/", letterboxd.BaseURL, slug))
	if err != nil {
		return err
	}
	stats, err := letterboxd.ParseFilmStats(slug, body)
	if err != nil {
		return err
	}
	return p.store.UpsertFilmStats(ctx, stats)
}

func (p *Pipeline) getPage(ctx context.Context, sem *semaphore.Weighted, url string) ([]byte, error) {
	if err := sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer sem.Release(1)
	return p.fetch.Get(ctx, url)
}

func (p *Pipeline) ensureStubs(ctx context.Context, watch []letterboxd.WatchEntry, diary []letterboxd.DiaryEntry) error {
	type stub struct {
		title string
		year  int32
	}
	seen := make(map[string]stub, len(watch)+len(diary))
	for _, e := range watch {
		seen[e.FilmSlug] = stub{e.Title, int32(e.Year)}
	}
	for _, e := range diary {
		if _, ok := seen[e.FilmSlug]; !ok {
			seen[e.FilmSlug] = stub{e.Title, int32(e.Year)}
		}
	}

	slugs := make([]string, 0, len(seen))
	titles := make([]string, 0, len(seen))
	years := make([]int32, 0, len(seen))
	for slug, s := range seen {
		slugs = append(slugs, slug)
		titles = append(titles, s.title)
		years = append(years, s.year)
	}
	return p.store.EnsureFilmStubs(ctx, slugs, titles, years)
}

func uniqueSlugs(watch []letterboxd.WatchEntry, diary []letterboxd.DiaryEntry) []string {
	seen := make(map[string]struct{}, len(watch)+len(diary))
	out := make([]string, 0, len(watch))
	for _, e := range watch {
		if _, ok := seen[e.FilmSlug]; !ok {
			seen[e.FilmSlug] = struct{}{}
			out = append(out, e.FilmSlug)
		}
	}
	for _, e := range diary {
		if _, ok := seen[e.FilmSlug]; !ok {
			seen[e.FilmSlug] = struct{}{}
			out = append(out, e.FilmSlug)
		}
	}
	return out
}

// dedupeWatch guards against a film appearing on two grid pages, which can
// happen when the user logs something while the scrape is paginating.
func dedupeWatch(in []letterboxd.WatchEntry) []letterboxd.WatchEntry {
	seen := make(map[string]struct{}, len(in))
	out := make([]letterboxd.WatchEntry, 0, len(in))
	for _, e := range in {
		if _, ok := seen[e.FilmSlug]; ok {
			continue
		}
		seen[e.FilmSlug] = struct{}{}
		out = append(out, e)
	}
	return out
}
