package letterboxd

import (
	"errors"
	"time"
)

// Sentinel errors. Callers classify retry behaviour on these.
var (
	// ErrNotFound means the resource is gone: a deleted film, a bad username.
	// Never retried.
	ErrNotFound = errors.New("letterboxd: not found")
	// ErrBlocked means Cloudflare rejected us (403/503 + challenge markers).
	// Retried with a longer backoff, and may rotate the browser profile.
	ErrBlocked = errors.New("letterboxd: blocked by bot protection")
	// ErrPrivate means the profile exists but its data is not public.
	ErrPrivate = errors.New("letterboxd: profile is private")
	// ErrTransient covers 5xx, timeouts and connection resets.
	ErrTransient = errors.New("letterboxd: transient upstream failure")
)

// Person is a director or a cast member.
type Person struct {
	Slug         string `json:"slug"`
	Name         string `json:"name"`
	BillingOrder int    `json:"billing_order,omitempty"`
}

// FilmDetails is the scraped metadata for one film. It is global data:
// identical for every user, cached in the films table and shared.
type FilmDetails struct {
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Year        int      `json:"year,omitempty"`
	RuntimeMin  int      `json:"runtime_min,omitempty"`
	Genres      []string `json:"genres,omitempty"`
	Themes      []string `json:"themes,omitempty"`
	Directors   []Person `json:"directors,omitempty"`
	Cast        []Person `json:"cast,omitempty"`
	Studios     []string `json:"studios,omitempty"`
	Countries   []string `json:"countries,omitempty"`
	Languages   []string `json:"languages,omitempty"`
	AvgRating   float64  `json:"avg_rating,omitempty"`
	RatingCount int64    `json:"rating_count,omitempty"`
}

// FilmStats comes from the lightweight /csi/film/{slug}/stats/ endpoint.
// These values drift continuously, so they carry a shorter cache TTL than
// the rest of FilmDetails.
type FilmStats struct {
	Slug       string `json:"slug"`
	WatchCount int64  `json:"watch_count"`
	LikeCount  int64  `json:"like_count"`
}

// WatchEntry is one row of a user's films grid: logged, but undated.
type WatchEntry struct {
	FilmSlug string   `json:"film_slug"`
	Title    string   `json:"title"`
	Year     int      `json:"year,omitempty"`
	Rating   *float64 `json:"rating,omitempty"`
	Liked    bool     `json:"liked"`
}

// DiaryEntry is one dated diary row. A film may appear many times (rewatches),
// which is why this is kept separate from WatchEntry.
type DiaryEntry struct {
	FilmSlug  string    `json:"film_slug"`
	Title     string    `json:"title"`
	Year      int       `json:"year,omitempty"`
	WatchedOn time.Time `json:"watched_on"`
	Rating    *float64  `json:"rating,omitempty"`
	Rewatch   bool      `json:"rewatch"`
}
