package store

import (
	"context"
	"fmt"
	"time"

	"github.com/danielkwan-dev/memento/internal/letterboxd"
	"github.com/jackc/pgx/v5"
)

// FetchVersion is bumped whenever the parser starts extracting a new field.
// Rows below the current version are treated as stale, so a new field can be
// backfilled without discarding the whole cache.
const FetchVersion = 1

// StaleSplit reports which slugs need scraping. This is the cache-miss diff
// that makes the global film table worth having: a returning user, or one with
// mainstream taste, may need almost no detail fetches at all.
type StaleSplit struct {
	NeedDetails []string
	NeedStats   []string
	CacheHits   int
}

// ClassifySlugs partitions slugs into those needing a details fetch, those
// needing only a stats refresh, and those fully cached.
//
// Details and stats have independent TTLs because they age differently: genres
// and runtime effectively never change, while watch counts drift continuously.
func (s *Store) ClassifySlugs(ctx context.Context, slugs []string, detailsTTL, statsTTL time.Duration) (*StaleSplit, error) {
	out := &StaleSplit{}
	if len(slugs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT
			s.slug,
			f.slug IS NOT NULL                                             AS exists,
			COALESCE(f.details_fetched_at, 'epoch'::timestamptz) < now() - $2::interval
				OR COALESCE(f.fetch_version, 0) < $3                       AS details_stale,
			COALESCE(f.stats_fetched_at, 'epoch'::timestamptz) < now() - $4::interval AS stats_stale
		FROM unnest($1::text[]) AS s(slug)
		LEFT JOIN films f ON f.slug = s.slug`,
		slugs, detailsTTL, FetchVersion, statsTTL)
	if err != nil {
		return nil, fmt.Errorf("classify slugs: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var slug string
		var exists, detailsStale, statsStale bool
		if err := rows.Scan(&slug, &exists, &detailsStale, &statsStale); err != nil {
			return nil, fmt.Errorf("scan classification: %w", err)
		}
		switch {
		case !exists || detailsStale:
			// A details fetch also refreshes the ratings, so no separate stats
			// entry is needed for these.
			out.NeedDetails = append(out.NeedDetails, slug)
		case statsStale:
			out.NeedStats = append(out.NeedStats, slug)
		default:
			out.CacheHits++
		}
	}
	return out, rows.Err()
}

// UpsertFilmDetails writes one film and replaces its association rows.
//
// The associations are delete-then-insert rather than merged: a film's genre
// list is small and authoritative per scrape, so replacing it is both simpler
// and correct when upstream removes a value.
func (s *Store) UpsertFilmDetails(ctx context.Context, d *letterboxd.FilmDetails) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin upsert %s: %w", d.Slug, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := upsertFilmRow(ctx, tx, d); err != nil {
		return err
	}
	if err := replaceAssociations(ctx, tx, d); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit upsert %s: %w", d.Slug, err)
	}
	return nil
}

func upsertFilmRow(ctx context.Context, tx pgx.Tx, d *letterboxd.FilmDetails) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO films (slug, title, year, runtime_min, avg_rating, rating_count,
		                   details_fetched_at, fetch_version)
		VALUES ($1, $2, NULLIF($3,0), NULLIF($4,0), NULLIF($5,0), NULLIF($6,0), now(), $7)
		ON CONFLICT (slug) DO UPDATE SET
			title              = EXCLUDED.title,
			year               = COALESCE(EXCLUDED.year, films.year),
			runtime_min        = COALESCE(EXCLUDED.runtime_min, films.runtime_min),
			avg_rating         = COALESCE(EXCLUDED.avg_rating, films.avg_rating),
			rating_count       = COALESCE(EXCLUDED.rating_count, films.rating_count),
			details_fetched_at = now(),
			fetch_version      = EXCLUDED.fetch_version`,
		d.Slug, d.Title, d.Year, d.RuntimeMin, d.AvgRating, d.RatingCount, FetchVersion)
	if err != nil {
		return fmt.Errorf("upsert film %s: %w", d.Slug, err)
	}
	return nil
}

func replaceAssociations(ctx context.Context, tx pgx.Tx, d *letterboxd.FilmDetails) error {
	simple := []struct {
		table  string
		column string
		values []string
	}{
		{"film_genres", "genre", d.Genres},
		{"film_themes", "theme", d.Themes},
		{"film_studios", "studio", d.Studios},
		{"film_countries", "country", d.Countries},
		{"film_languages", "language", d.Languages},
	}
	for _, a := range simple {
		if _, err := tx.Exec(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE film_slug = $1`, a.table), d.Slug); err != nil {
			return fmt.Errorf("clear %s for %s: %w", a.table, d.Slug, err)
		}
		if len(a.values) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`INSERT INTO %s (film_slug, %s) SELECT $1, v FROM unnest($2::text[]) AS v
			 ON CONFLICT DO NOTHING`, a.table, a.column), d.Slug, a.values); err != nil {
			return fmt.Errorf("insert %s for %s: %w", a.table, d.Slug, err)
		}
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM film_people WHERE film_slug = $1`, d.Slug); err != nil {
		return fmt.Errorf("clear film_people for %s: %w", d.Slug, err)
	}

	people := make([]letterboxd.Person, 0, len(d.Directors)+len(d.Cast))
	roles := make([]string, 0, cap(people))
	for _, p := range d.Directors {
		people = append(people, p)
		roles = append(roles, "director")
	}
	for _, p := range d.Cast {
		people = append(people, p)
		roles = append(roles, "actor")
	}
	if len(people) == 0 {
		return nil
	}

	slugs := make([]string, len(people))
	names := make([]string, len(people))
	orders := make([]int32, len(people))
	for i, p := range people {
		slugs[i] = p.Slug
		names[i] = p.Name
		orders[i] = int32(p.BillingOrder)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO film_people (film_slug, person_slug, name, role, billing_order)
		SELECT $1, s, n, r::person_role, NULLIF(o,0)
		FROM unnest($2::text[], $3::text[], $4::text[], $5::int[]) AS t(s, n, r, o)
		ON CONFLICT (film_slug, person_slug, role) DO UPDATE SET
			name          = EXCLUDED.name,
			billing_order = EXCLUDED.billing_order`,
		d.Slug, slugs, names, roles, orders); err != nil {
		return fmt.Errorf("insert film_people for %s: %w", d.Slug, err)
	}
	return nil
}

// UpsertFilmStats refreshes only the popularity counters, leaving metadata and
// details_fetched_at untouched.
func (s *Store) UpsertFilmStats(ctx context.Context, st *letterboxd.FilmStats) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO films (slug, title, watch_count, like_count, stats_fetched_at)
		VALUES ($1, $1, $2, $3, now())
		ON CONFLICT (slug) DO UPDATE SET
			watch_count      = EXCLUDED.watch_count,
			like_count       = EXCLUDED.like_count,
			stats_fetched_at = now()`,
		st.Slug, st.WatchCount, st.LikeCount)
	if err != nil {
		return fmt.Errorf("upsert stats %s: %w", st.Slug, err)
	}
	return nil
}

// EnsureFilmStubs inserts placeholder rows so watch/diary entries can reference
// films whose details have not been fetched yet, keeping the foreign keys valid.
func (s *Store) EnsureFilmStubs(ctx context.Context, slugs, titles []string, years []int32) error {
	if len(slugs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO films (slug, title, year)
		SELECT s, t, NULLIF(y, 0)
		FROM unnest($1::text[], $2::text[], $3::int[]) AS f(s, t, y)
		ON CONFLICT (slug) DO UPDATE SET
			title = CASE WHEN films.details_fetched_at IS NULL
			             THEN EXCLUDED.title ELSE films.title END,
			year  = COALESCE(films.year, EXCLUDED.year)`,
		slugs, titles, years)
	if err != nil {
		return fmt.Errorf("ensure film stubs: %w", err)
	}
	return nil
}
