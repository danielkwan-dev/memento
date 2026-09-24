package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/danielkwan-dev/memento/internal/letterboxd"
	"github.com/jackc/pgx/v5"
)

// UpsertUser returns the user's id, creating the row if needed. Usernames are
// matched case-insensitively because Letterboxd URLs are.
func (s *Store) UpsertUser(ctx context.Context, username string) (int64, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (username) VALUES ($1)
		ON CONFLICT (username) DO UPDATE SET username = EXCLUDED.username
		RETURNING id`, username).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert user %s: %w", username, err)
	}
	return id, nil
}

func (s *Store) UserID(ctx context.Context, username string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE username = $1`,
		strings.ToLower(strings.TrimSpace(username))).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("lookup user: %w", err)
	}
	return id, true, nil
}

func (s *Store) MarkUserSynced(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE users SET last_synced_at = now() WHERE id = $1`, userID)
	if err != nil {
		return fmt.Errorf("mark synced: %w", err)
	}
	return nil
}

// LastSyncedAt reports when the user was last scraped, so the API can offer a
// cached view instead of re-scraping.
func (s *Store) LastSyncedAt(ctx context.Context, username string) (time.Time, bool, error) {
	var t *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT last_synced_at FROM users WHERE username = $1`,
		strings.ToLower(strings.TrimSpace(username))).Scan(&t)
	if err != nil {
		if isNoRows(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("last synced: %w", err)
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return *t, true, nil
}

// ReplaceWatchEntries swaps in a user's full films-grid snapshot atomically.
//
// A scrape always yields the complete grid, so replacing wholesale is what
// makes un-logged films actually disappear. Doing it in one transaction means
// readers never observe a half-empty profile.
func (s *Store) ReplaceWatchEntries(ctx context.Context, userID int64, entries []letterboxd.WatchEntry) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin replace watch entries: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM watch_entries WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("clear watch entries: %w", err)
	}

	if len(entries) > 0 {
		slugs := make([]string, len(entries))
		ratings := make([]*float64, len(entries))
		liked := make([]bool, len(entries))
		for i, e := range entries {
			slugs[i] = e.FilmSlug
			ratings[i] = e.Rating
			liked[i] = e.Liked
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO watch_entries (user_id, film_slug, rating, liked)
			SELECT $1, s, r, l
			FROM unnest($2::text[], $3::numeric[], $4::bool[]) AS t(s, r, l)
			ON CONFLICT (user_id, film_slug) DO UPDATE SET
				rating = EXCLUDED.rating, liked = EXCLUDED.liked`,
			userID, slugs, ratings, liked); err != nil {
			return fmt.Errorf("insert watch entries: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit watch entries: %w", err)
	}
	return nil
}

// ReplaceDiaryEntries swaps in a user's full diary snapshot atomically.
func (s *Store) ReplaceDiaryEntries(ctx context.Context, userID int64, entries []letterboxd.DiaryEntry) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin replace diary: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM diary_entries WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("clear diary: %w", err)
	}

	if len(entries) > 0 {
		slugs := make([]string, len(entries))
		dates := make([]time.Time, len(entries))
		ratings := make([]*float64, len(entries))
		rewatch := make([]bool, len(entries))
		for i, e := range entries {
			slugs[i] = e.FilmSlug
			dates[i] = e.WatchedOn
			ratings[i] = e.Rating
			rewatch[i] = e.Rewatch
		}
		// Letterboxd permits several viewings of one film on a single day; the
		// unique constraint collapses them, so DO NOTHING is correct here.
		if _, err := tx.Exec(ctx, `
			INSERT INTO diary_entries (user_id, film_slug, watched_on, rating, rewatch)
			SELECT $1, s, d, r, w
			FROM unnest($2::text[], $3::date[], $4::numeric[], $5::bool[]) AS t(s, d, r, w)
			ON CONFLICT (user_id, film_slug, watched_on) DO NOTHING`,
			userID, slugs, dates, ratings, rewatch); err != nil {
			return fmt.Errorf("insert diary: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit diary: %w", err)
	}
	return nil
}

// DeleteUser removes a user and, by cascade, their watch and diary entries and
// cached stats. The global films table is deliberately untouched: it holds only
// public facts about films, never anything personal.
func (s *Store) DeleteUser(ctx context.Context, username string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE username = $1`,
		strings.ToLower(strings.TrimSpace(username)))
	if err != nil {
		return false, fmt.Errorf("delete user: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
