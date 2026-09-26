package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/danielkwan-dev/memento/internal/letterboxd"
	"github.com/danielkwan-dev/memento/internal/store"
)

// RunImport ingests a Letterboxd data export instead of scraping the user's pages.
//
// This shares the hydrate and persist phases with Run; only the index phase
// differs. It matters for two reasons: the paginated user pages are the most
// aggressively blocked requests in the whole scrape, so skipping them removes the
// main failure mode, and it gives the app a working path when scraping is blocked
// outright.
func (p *Pipeline) RunImport(ctx context.Context, jobID uuid.UUID, username string, zipData []byte) (*Result, error) {
	res := &Result{}

	// --- Phase: resolve (parse the archive) ---
	if err := p.store.UpdateProgress(ctx, jobID, store.PhaseResolve, 0, 0, 0); err != nil {
		return nil, err
	}
	exp, err := letterboxd.ParseExportZip(zipData)
	if err != nil {
		return nil, err
	}

	userID, err := p.store.UpsertUser(ctx, username)
	if err != nil {
		return nil, err
	}
	res.UserID = userID

	// --- Phase: index (already done: it came from the file) ---
	if err := p.store.UpdateProgress(ctx, jobID, store.PhaseIndex, 0, 0, 0); err != nil {
		return nil, err
	}
	watch := dedupeWatch(exp.Watched)
	diary := exp.Diary
	res.WatchEntries = len(watch)
	res.DiaryEntries = len(diary)

	if len(watch) == 0 && len(diary) == 0 {
		return nil, fmt.Errorf("the export contained no films")
	}

	if err := p.ensureStubs(ctx, watch, diary); err != nil {
		return nil, err
	}

	// --- Phase: hydrate (identical to the scrape path) ---
	slugs := uniqueSlugs(watch, diary)
	split, err := p.store.ClassifySlugs(ctx, slugs, p.cfg.DetailsTTL, p.cfg.StatsTTL)
	if err != nil {
		return nil, err
	}
	res.CacheHits = split.CacheHits

	total := len(split.NeedDetails) + len(split.NeedStats)
	p.log.Info("import hydrate plan", "username", username, "films", len(slugs),
		"need_details", len(split.NeedDetails), "need_stats", len(split.NeedStats),
		"cache_hits", split.CacheHits)

	if err := p.store.UpdateProgress(ctx, jobID, store.PhaseHydrate, 0, total, split.CacheHits); err != nil {
		return nil, err
	}

	// Hydration is ENRICHMENT for an import, not the data itself.
	//
	// The film list came from the user's own export, so it is already complete and
	// correct -- fetching details only adds genres, runtimes and cast. If upstream
	// throttles partway, throwing the whole import away would discard a perfectly
	// good film list over missing metadata, and tell the user to "upload an export
	// instead" when that is exactly what they just did. So a hydrate failure here
	// degrades the result and the job still succeeds; charts that need the missing
	// metadata simply show less, and a later run fills the gaps from the cache.
	//
	// A scrape is different: there the film list itself comes from the network, so
	// widespread failure means the data is untrustworthy and Run still fails.
	fetched, statsFetched, failures, err := p.hydrate(ctx, jobID, split, total)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		p.log.Warn("import hydration incomplete; persisting the export anyway",
			"username", username, "err", err)
	}
	res.FilmsFetched = fetched
	res.StatsFetched = statsFetched
	res.FilmFailures = failures

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

	p.log.Info("import complete", "username", username,
		"watch", res.WatchEntries, "diary", res.DiaryEntries,
		"fetched", res.FilmsFetched, "cache_hits", res.CacheHits,
		"failures", res.FilmFailures)
	return res, nil
}
