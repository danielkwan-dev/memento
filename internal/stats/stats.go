// Package stats computes the chart payloads as SQL aggregations.
//
// Doing this in the database rather than in application code keeps the whole
// dataset out of process memory, and lets Postgres use the indexes on the
// association tables. Each category is one query returning small JSON.
package stats

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Category names the chart groups the API exposes.
type Category string

const (
	CatRatings   Category = "ratings"
	CatActivity  Category = "activity"
	CatGenres    Category = "genres"
	CatThemes    Category = "themes"
	CatDecades   Category = "decades"
	CatObscurity Category = "obscurity"
	CatRuntime   Category = "runtime"
	CatPeople    Category = "people"
	CatGeography Category = "geography"
	CatOverview  Category = "overview"
)

// AllCategories is the canonical order the UI renders in.
var AllCategories = []Category{
	CatOverview, CatRatings, CatActivity, CatGenres, CatThemes,
	CatDecades, CatObscurity, CatRuntime, CatPeople, CatGeography,
}

func ParseCategory(s string) (Category, bool) {
	for _, c := range AllCategories {
		if string(c) == s {
			return c, true
		}
	}
	return "", false
}

type Service struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// --- payload shapes ---

// Bucket is a labelled count, optionally with the user's average rating for
// that label. Most charts are lists of these.
type Bucket struct {
	Label     string   `json:"label"`
	Count     int      `json:"count"`
	AvgRating *float64 `json:"avg_rating,omitempty"`
}

type RatingBucket struct {
	Rating float64 `json:"rating"`
	Count  int     `json:"count"`
}

// ScatterPoint is one film plotted as the user's rating against Letterboxd's
// average, which shows where their taste diverges from the crowd.
type ScatterPoint struct {
	Title     string  `json:"title"`
	Year      int     `json:"year,omitempty"`
	Rating    float64 `json:"rating"`
	AvgRating float64 `json:"avg_rating"`
}

// LikedSplit powers the liked/not-liked pie.
type LikedSplit struct {
	Liked    int `json:"liked"`
	NotLiked int `json:"not_liked"`
}

// RatingsPayload is everything the ratings tab needs in one response.
type RatingsPayload struct {
	Histogram []RatingBucket `json:"histogram"`
	Scatter   []ScatterPoint `json:"scatter"`
	Liked     LikedSplit     `json:"liked"`
}

type DayCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// RareFilm is one of the user's least-watched films, for the obscurity list.
type RareFilm struct {
	Title      string `json:"title"`
	WatchCount int64  `json:"watch_count"`
}

type Overview struct {
	FilmsLogged   int      `json:"films_logged"`
	DiaryEntries  int      `json:"diary_entries"`
	Rated         int      `json:"rated"`
	Liked         int      `json:"liked"`
	AvgRating     *float64 `json:"avg_rating,omitempty"`
	TotalRuntime  int      `json:"total_runtime_min"`
	DistinctYears int      `json:"distinct_years"`
	Rewatches     int      `json:"rewatches"`
}

// Compute dispatches to the query for one category.
func (s *Service) Compute(ctx context.Context, userID int64, cat Category) (any, error) {
	switch cat {
	case CatOverview:
		return s.overview(ctx, userID)
	case CatRatings:
		return s.ratingsPayload(ctx, userID)
	case CatActivity:
		return s.activity(ctx, userID)
	case CatGenres:
		return s.association(ctx, userID, "film_genres", "genre", 30)
	case CatThemes:
		return s.association(ctx, userID, "film_themes", "theme", 30)
	case CatDecades:
		return s.decades(ctx, userID)
	case CatObscurity:
		return s.obscurity(ctx, userID)
	case CatRuntime:
		return s.runtime(ctx, userID)
	case CatPeople:
		return s.people(ctx, userID)
	case CatGeography:
		return s.geography(ctx, userID)
	default:
		return nil, fmt.Errorf("unknown category %q", cat)
	}
}

func (s *Service) overview(ctx context.Context, userID int64) (*Overview, error) {
	var o Overview
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM watch_entries WHERE user_id = $1),
			(SELECT count(*) FROM diary_entries WHERE user_id = $1),
			(SELECT count(*) FROM watch_entries WHERE user_id = $1 AND rating IS NOT NULL),
			(SELECT count(*) FROM watch_entries WHERE user_id = $1 AND liked),
			(SELECT avg(rating)::float8 FROM watch_entries WHERE user_id = $1 AND rating IS NOT NULL),
			(SELECT COALESCE(sum(f.runtime_min), 0)
			   FROM watch_entries w JOIN films f ON f.slug = w.film_slug
			  WHERE w.user_id = $1),
			(SELECT count(DISTINCT f.year)
			   FROM watch_entries w JOIN films f ON f.slug = w.film_slug
			  WHERE w.user_id = $1 AND f.year IS NOT NULL),
			(SELECT count(*) FROM diary_entries WHERE user_id = $1 AND rewatch)`,
		userID).Scan(&o.FilmsLogged, &o.DiaryEntries, &o.Rated, &o.Liked,
		&o.AvgRating, &o.TotalRuntime, &o.DistinctYears, &o.Rewatches)
	if err != nil {
		return nil, fmt.Errorf("overview: %w", err)
	}
	return &o, nil
}

// ratings is the star-distribution histogram. Ratings are NUMERIC(2,1), so
// grouping is exact -- with floats, 4.5 could land in two different buckets.
func (s *Service) ratings(ctx context.Context, userID int64) ([]RatingBucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT rating::float8, count(*)::int
		FROM watch_entries
		WHERE user_id = $1 AND rating IS NOT NULL
		GROUP BY rating
		ORDER BY rating`, userID)
	if err != nil {
		return nil, fmt.Errorf("ratings: %w", err)
	}
	defer rows.Close()

	// Pre-seed every half-star so the chart shows empty buckets as gaps rather
	// than omitting them.
	idx := map[float64]int{}
	out := make([]RatingBucket, 0, 10)
	for r := 0.5; r <= 5.0; r += 0.5 {
		idx[r] = len(out)
		out = append(out, RatingBucket{Rating: r})
	}
	for rows.Next() {
		var r float64
		var c int
		if err := rows.Scan(&r, &c); err != nil {
			return nil, err
		}
		if i, ok := idx[r]; ok {
			out[i].Count = c
		}
	}
	return out, rows.Err()
}

// ratingsPayload bundles the ratings charts, so the tab needs one request.
func (s *Service) ratingsPayload(ctx context.Context, userID int64) (*RatingsPayload, error) {
	hist, err := s.ratings(ctx, userID)
	if err != nil {
		return nil, err
	}
	scatter, err := s.ratingScatter(ctx, userID)
	if err != nil {
		return nil, err
	}
	liked, err := s.likedSplit(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &RatingsPayload{Histogram: hist, Scatter: scatter, Liked: *liked}, nil
}

// ratingScatter pairs the user's rating with the crowd's for each rated film.
//
// Capped: a scatter of 5000 points is slow to render and no more readable than
// 1200, and the shape is what matters here.
func (s *Service) ratingScatter(ctx context.Context, userID int64) ([]ScatterPoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT f.title, COALESCE(f.year, 0), w.rating::float8, f.avg_rating::float8
		FROM watch_entries w
		JOIN films f ON f.slug = w.film_slug
		WHERE w.user_id = $1 AND w.rating IS NOT NULL AND f.avg_rating IS NOT NULL
		ORDER BY f.avg_rating
		LIMIT 1200`, userID)
	if err != nil {
		return nil, fmt.Errorf("rating scatter: %w", err)
	}
	defer rows.Close()

	var out []ScatterPoint
	for rows.Next() {
		var p ScatterPoint
		if err := rows.Scan(&p.Title, &p.Year, &p.Rating, &p.AvgRating); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) likedSplit(ctx context.Context, userID int64) (*LikedSplit, error) {
	var l LikedSplit
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE liked), count(*) FILTER (WHERE NOT liked)
		FROM watch_entries WHERE user_id = $1`, userID).Scan(&l.Liked, &l.NotLiked)
	if err != nil {
		return nil, fmt.Errorf("liked split: %w", err)
	}
	return &l, nil
}

// activity powers the calendar heatmap and the per-month line chart.
func (s *Service) activity(ctx context.Context, userID int64) (map[string]any, error) {
	daily, err := s.dayCounts(ctx, userID, `
		SELECT to_char(watched_on, 'YYYY-MM-DD'), count(*)::int
		FROM diary_entries WHERE user_id = $1
		GROUP BY watched_on ORDER BY watched_on`)
	if err != nil {
		return nil, err
	}
	monthly, err := s.dayCounts(ctx, userID, `
		SELECT to_char(date_trunc('month', watched_on), 'YYYY-MM'), count(*)::int
		FROM diary_entries WHERE user_id = $1
		GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	// Average rating per month, for the ratings-over-time line.
	trend, err := s.ratingTrend(ctx, userID)
	if err != nil {
		return nil, err
	}

	weekday, err := s.dayCounts(ctx, userID, `
		SELECT trim(to_char(watched_on, 'Day')), count(*)::int
		FROM diary_entries WHERE user_id = $1
		GROUP BY 1, extract(isodow from watched_on) ORDER BY extract(isodow from watched_on)`)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"daily": daily, "monthly": monthly, "weekday": weekday, "rating_trend": trend,
	}, nil
}

// MonthAvg is the mean rating for one month of diary entries.
type MonthAvg struct {
	Month string  `json:"month"`
	Avg   float64 `json:"avg"`
	Count int     `json:"count"`
}

func (s *Service) ratingTrend(ctx context.Context, userID int64) ([]MonthAvg, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT to_char(date_trunc('month', watched_on), 'YYYY-MM'),
		       avg(rating)::float8, count(*)::int
		FROM diary_entries
		WHERE user_id = $1 AND rating IS NOT NULL
		GROUP BY 1 ORDER BY 1`, userID)
	if err != nil {
		return nil, fmt.Errorf("rating trend: %w", err)
	}
	defer rows.Close()
	var out []MonthAvg
	for rows.Next() {
		var m MonthAvg
		if err := rows.Scan(&m.Month, &m.Avg, &m.Count); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Service) dayCounts(ctx context.Context, userID int64, query string) ([]DayCount, error) {
	rows, err := s.pool.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("activity: %w", err)
	}
	defer rows.Close()
	var out []DayCount
	for rows.Next() {
		var d DayCount
		if err := rows.Scan(&d.Date, &d.Count); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// association handles every many-to-many chart (genres, themes, and the
// geography tables) with one parameterised query.
//
// The table and column names are interpolated, never user input: they come from
// the fixed call sites below.
func (s *Service) association(ctx context.Context, userID int64, table, column string, limit int) ([]Bucket, error) {
	query := fmt.Sprintf(`
		SELECT a.%[2]s, count(*)::int,
		       avg(w.rating) FILTER (WHERE w.rating IS NOT NULL)::float8
		FROM watch_entries w
		JOIN %[1]s a ON a.film_slug = w.film_slug
		WHERE w.user_id = $1
		GROUP BY a.%[2]s
		ORDER BY count(*) DESC, a.%[2]s
		LIMIT $2`, table, column)
	return s.buckets(ctx, query, userID, limit)
}

func (s *Service) buckets(ctx context.Context, query string, args ...any) ([]Bucket, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("buckets: %w", err)
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Label, &b.Count, &b.AvgRating); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Service) decades(ctx context.Context, userID int64) ([]Bucket, error) {
	return s.buckets(ctx, `
		SELECT ((f.year / 10) * 10)::text || 's', count(*)::int,
		       avg(w.rating) FILTER (WHERE w.rating IS NOT NULL)::float8
		FROM watch_entries w
		JOIN films f ON f.slug = w.film_slug
		WHERE w.user_id = $1 AND f.year IS NOT NULL
		GROUP BY (f.year / 10) * 10
		ORDER BY (f.year / 10) * 10`, userID)
}

// obscurity ranks the user's films against the whole films corpus by watch
// count. This is only possible because film data is cached globally, and it
// sharpens as the cache grows.
func (s *Service) obscurity(ctx context.Context, userID int64) (map[string]any, error) {
	rows, err := s.pool.Query(ctx, `
		WITH ranked AS (
			SELECT slug, watch_count,
			       percent_rank() OVER (ORDER BY watch_count) AS pct
			FROM films
			WHERE watch_count IS NOT NULL
		)
		SELECT width_bucket(r.pct, 0, 1, 10) AS decile, count(*)::int
		FROM watch_entries w
		JOIN ranked r ON r.slug = w.film_slug
		WHERE w.user_id = $1
		GROUP BY decile ORDER BY decile`, userID)
	if err != nil {
		return nil, fmt.Errorf("obscurity: %w", err)
	}
	defer rows.Close()

	deciles := make([]Bucket, 10)
	for i := range deciles {
		deciles[i] = Bucket{Label: fmt.Sprintf("%d-%d%%", i*10, (i+1)*10)}
	}
	for rows.Next() {
		var d, c int
		if err := rows.Scan(&d, &c); err != nil {
			return nil, err
		}
		// width_bucket returns 1..10, and 11 for an exact 1.0 upper bound.
		if d >= 1 && d <= 10 {
			deciles[d-1].Count += c
		} else if d > 10 {
			deciles[9].Count += c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The most obscure films the user has logged, which makes a good list.
	rarest, err := s.pool.Query(ctx, `
		SELECT f.title, f.watch_count
		FROM watch_entries w
		JOIN films f ON f.slug = w.film_slug
		WHERE w.user_id = $1 AND f.watch_count IS NOT NULL
		ORDER BY f.watch_count ASC
		LIMIT 10`, userID)
	if err != nil {
		return nil, fmt.Errorf("obscurity rarest: %w", err)
	}
	defer rarest.Close()

	var rares []RareFilm
	for rarest.Next() {
		var r RareFilm
		if err := rarest.Scan(&r.Title, &r.WatchCount); err != nil {
			return nil, err
		}
		rares = append(rares, r)
	}
	return map[string]any{"deciles": deciles, "rarest": rares}, rarest.Err()
}

func (s *Service) runtime(ctx context.Context, userID int64) ([]Bucket, error) {
	// 30-minute bands, with everything past 240 minutes collapsed into one tail
	// bucket so a single 8-hour film does not stretch the axis.
	return s.buckets(ctx, `
		SELECT CASE
		         WHEN f.runtime_min >= 240 THEN '240+'
		         ELSE ((f.runtime_min / 30) * 30)::text || '-' || ((f.runtime_min / 30) * 30 + 29)::text
		       END,
		       count(*)::int,
		       avg(w.rating) FILTER (WHERE w.rating IS NOT NULL)::float8
		FROM watch_entries w
		JOIN films f ON f.slug = w.film_slug
		WHERE w.user_id = $1 AND f.runtime_min IS NOT NULL AND f.runtime_min > 0
		GROUP BY 1, LEAST(f.runtime_min / 30, 8)
		ORDER BY LEAST(f.runtime_min / 30, 8)`, userID)
}

func (s *Service) people(ctx context.Context, userID int64) (map[string]any, error) {
	byRole := func(role string, limit int) ([]Bucket, error) {
		return s.buckets(ctx, `
			SELECT p.name, count(*)::int,
			       avg(w.rating) FILTER (WHERE w.rating IS NOT NULL)::float8
			FROM watch_entries w
			JOIN film_people p ON p.film_slug = w.film_slug
			WHERE w.user_id = $1 AND p.role = $2::person_role
			GROUP BY p.person_slug, p.name
			ORDER BY count(*) DESC, p.name
			LIMIT $3`, userID, role, limit)
	}
	directors, err := byRole("director", 25)
	if err != nil {
		return nil, err
	}
	actors, err := byRole("actor", 25)
	if err != nil {
		return nil, err
	}
	studios, err := s.association(ctx, userID, "film_studios", "studio", 25)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"directors": directors, "actors": actors, "studios": studios,
	}, nil
}

func (s *Service) geography(ctx context.Context, userID int64) (map[string]any, error) {
	countries, err := s.association(ctx, userID, "film_countries", "country", 100)
	if err != nil {
		return nil, err
	}
	languages, err := s.association(ctx, userID, "film_languages", "language", 50)
	if err != nil {
		return nil, err
	}
	return map[string]any{"countries": countries, "languages": languages}, nil
}
