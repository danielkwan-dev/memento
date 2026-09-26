package stats

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests need real Postgres: the thing under test is the SQL. A typo in a
// window function or a wrong GROUP BY would otherwise only surface in production.
//
//	docker compose up -d postgres
//	MEMENTO_TEST_DATABASE_URL=postgres://memento:memento@localhost:5432/memento?sslmode=disable go test ./internal/stats/
func testService(t *testing.T) (*Service, *pgxpool.Pool, int64) {
	t.Helper()
	dsn := os.Getenv("MEMENTO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MEMENTO_TEST_DATABASE_URL to run stats tests (docker compose up -d postgres)")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// The store package owns migrations; this asserts they have been applied
	// rather than duplicating them.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'watch_entries')`).
		Scan(&exists); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if !exists {
		t.Skip("schema not migrated; run the store tests or start the api once first")
	}

	if _, err := pool.Exec(ctx,
		`TRUNCATE jobs, user_stats, diary_entries, watch_entries, users, films CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	userID := seed(t, pool)
	return New(pool), pool, userID
}

// seed builds a small but realistic dataset: three films with full metadata,
// ratings, likes, a rewatch, and widely different watch counts so the obscurity
// percentile has something to rank.
func seed(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()

	var userID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('tester') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	films := []struct {
		slug       string
		title      string
		year       int
		runtime    int
		watchCount int64
		genres     []string
		country    string
		language   string
		studio     string
		director   string
		theme      string
	}{
		{"popular-film", "Popular Film", 2019, 120, 5_000_000,
			[]string{"Drama", "Thriller"}, "USA", "English", "Big Studio", "dir-a", "Tense"},
		{"mid-film", "Mid Film", 1995, 95, 50_000,
			[]string{"Comedy"}, "France", "French", "Small Studio", "dir-b", "Wry"},
		{"obscure-film", "Obscure Film", 1972, 210, 400,
			[]string{"Drama"}, "Japan", "Japanese", "Tiny Studio", "dir-a", "Tense"},
	}

	for _, f := range films {
		if _, err := pool.Exec(ctx, `
			INSERT INTO films (slug, title, year, runtime_min, watch_count, like_count,
			                   avg_rating, rating_count, details_fetched_at, stats_fetched_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8, now(), now())`,
			f.slug, f.title, f.year, f.runtime, f.watchCount, f.watchCount/10, 4.0, 1000); err != nil {
			t.Fatalf("seed film %s: %v", f.slug, err)
		}
		for _, g := range f.genres {
			if _, err := pool.Exec(ctx,
				`INSERT INTO film_genres (film_slug, genre) VALUES ($1,$2)`, f.slug, g); err != nil {
				t.Fatalf("seed genre: %v", err)
			}
		}
		for table, col := range map[string]string{
			"film_countries": f.country,
			"film_languages": f.language,
			"film_studios":   f.studio,
			"film_themes":    f.theme,
		} {
			colName := map[string]string{
				"film_countries": "country", "film_languages": "language",
				"film_studios": "studio", "film_themes": "theme",
			}[table]
			if _, err := pool.Exec(ctx,
				`INSERT INTO `+table+` (film_slug, `+colName+`) VALUES ($1,$2)`,
				f.slug, col); err != nil {
				t.Fatalf("seed %s: %v", table, err)
			}
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO film_people (film_slug, person_slug, name, role, billing_order)
			VALUES ($1, $2, $3, 'director', NULL), ($1, 'actor-x', 'Actor X', 'actor', 1)`,
			f.slug, f.director, "Director "+f.director); err != nil {
			t.Fatalf("seed people: %v", err)
		}
	}

	// Ratings: 4.5, 3.0, and one unrated. Two liked.
	if _, err := pool.Exec(ctx, `
		INSERT INTO watch_entries (user_id, film_slug, rating, liked) VALUES
			($1, 'popular-film', 4.5, true),
			($1, 'mid-film',     3.0, true),
			($1, 'obscure-film', NULL, false)`, userID); err != nil {
		t.Fatalf("seed watch entries: %v", err)
	}

	// Diary: one film watched twice (the second a rewatch), on known weekdays.
	if _, err := pool.Exec(ctx, `
		INSERT INTO diary_entries (user_id, film_slug, watched_on, rating, rewatch) VALUES
			($1, 'popular-film', '2024-03-04', 4.5, false),
			($1, 'popular-film', '2024-03-11', 4.5, true),
			($1, 'mid-film',     '2024-04-02', 3.0, false)`, userID); err != nil {
		t.Fatalf("seed diary: %v", err)
	}
	return userID
}

func TestOverview(t *testing.T) {
	svc, _, userID := testService(t)

	o, err := svc.overview(context.Background(), userID)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}

	if o.FilmsLogged != 3 {
		t.Errorf("FilmsLogged = %d, want 3", o.FilmsLogged)
	}
	if o.DiaryEntries != 3 {
		t.Errorf("DiaryEntries = %d, want 3", o.DiaryEntries)
	}
	if o.Rated != 2 {
		t.Errorf("Rated = %d, want 2 (one film is unrated)", o.Rated)
	}
	if o.Liked != 2 {
		t.Errorf("Liked = %d, want 2", o.Liked)
	}
	if o.AvgRating == nil {
		t.Fatal("AvgRating is nil")
	}
	// (4.5 + 3.0) / 2, ignoring the unrated film.
	if *o.AvgRating < 3.74 || *o.AvgRating > 3.76 {
		t.Errorf("AvgRating = %v, want 3.75", *o.AvgRating)
	}
	if o.TotalRuntime != 120+95+210 {
		t.Errorf("TotalRuntime = %d, want 425", o.TotalRuntime)
	}
	if o.DistinctYears != 3 {
		t.Errorf("DistinctYears = %d, want 3", o.DistinctYears)
	}
	if o.Rewatches != 1 {
		t.Errorf("Rewatches = %d, want 1", o.Rewatches)
	}
}

// Every half-star must be present so the chart shows empty buckets as gaps.
func TestRatings_SeedsAllHalfStars(t *testing.T) {
	svc, _, userID := testService(t)

	buckets, err := svc.ratings(context.Background(), userID)
	if err != nil {
		t.Fatalf("ratings: %v", err)
	}
	if len(buckets) != 10 {
		t.Fatalf("got %d buckets, want 10 (0.5 through 5.0)", len(buckets))
	}

	byRating := map[float64]int{}
	for _, b := range buckets {
		byRating[b.Rating] = b.Count
	}
	if byRating[4.5] != 1 {
		t.Errorf("4.5 bucket = %d, want 1", byRating[4.5])
	}
	if byRating[3.0] != 1 {
		t.Errorf("3.0 bucket = %d, want 1", byRating[3.0])
	}
	if byRating[5.0] != 0 {
		t.Errorf("5.0 bucket = %d, want 0", byRating[5.0])
	}
	// Ordering matters: the histogram renders in array order.
	for i := 1; i < len(buckets); i++ {
		if buckets[i].Rating <= buckets[i-1].Rating {
			t.Errorf("buckets out of order at %d: %v then %v",
				i, buckets[i-1].Rating, buckets[i].Rating)
		}
	}
}

func TestGenres_CountsAndAverages(t *testing.T) {
	svc, _, userID := testService(t)

	rows, err := svc.association(context.Background(), userID, "film_genres", "genre", 30)
	if err != nil {
		t.Fatalf("genres: %v", err)
	}

	byLabel := map[string]Bucket{}
	for _, r := range rows {
		byLabel[r.Label] = r
	}
	// Drama is on the popular film and the obscure one.
	if got := byLabel["Drama"].Count; got != 2 {
		t.Errorf("Drama count = %d, want 2", got)
	}
	if got := byLabel["Comedy"].Count; got != 1 {
		t.Errorf("Comedy count = %d, want 1", got)
	}
	// Only the rated Drama film contributes to the average, so it is 4.5 not 2.25.
	if avg := byLabel["Drama"].AvgRating; avg == nil {
		t.Error("Drama AvgRating is nil")
	} else if *avg < 4.49 || *avg > 4.51 {
		t.Errorf("Drama AvgRating = %v, want 4.5 (unrated films excluded)", *avg)
	}
	// Ranked by count descending.
	if len(rows) > 1 && rows[0].Count < rows[1].Count {
		t.Errorf("genres not ordered by count desc: %v", rows)
	}
}

func TestDecades(t *testing.T) {
	svc, _, userID := testService(t)

	rows, err := svc.decades(context.Background(), userID)
	if err != nil {
		t.Fatalf("decades: %v", err)
	}
	labels := make([]string, len(rows))
	for i, r := range rows {
		labels[i] = r.Label
	}
	// 1972 -> 1970s, 1995 -> 1990s, 2019 -> 2010s, ordered chronologically.
	want := []string{"1970s", "1990s", "2010s"}
	if len(labels) != len(want) {
		t.Fatalf("decades = %v, want %v", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("decade[%d] = %s, want %s", i, labels[i], want[i])
		}
	}
}

// The obscurity percentile is the query that most justifies the global cache.
func TestObscurity(t *testing.T) {
	svc, _, userID := testService(t)

	out, err := svc.obscurity(context.Background(), userID)
	if err != nil {
		t.Fatalf("obscurity: %v", err)
	}

	deciles, ok := out["deciles"].([]Bucket)
	if !ok {
		t.Fatalf("deciles has unexpected type %T", out["deciles"])
	}
	if len(deciles) != 10 {
		t.Errorf("got %d deciles, want 10", len(deciles))
	}
	var total int
	for _, d := range deciles {
		total += d.Count
	}
	if total != 3 {
		t.Errorf("deciles total = %d, want 3 films", total)
	}

	rarest, ok := out["rarest"].([]RareFilm)
	if !ok {
		t.Fatalf("rarest has unexpected type %T", out["rarest"])
	}
	if len(rarest) == 0 {
		t.Fatal("rarest is empty")
	}
	// Ascending watch count: the least-watched film first.
	if rarest[0].Title != "Obscure Film" {
		t.Errorf("rarest[0] = %q, want Obscure Film", rarest[0].Title)
	}
	for i := 1; i < len(rarest); i++ {
		if rarest[i].WatchCount < rarest[i-1].WatchCount {
			t.Errorf("rarest not ascending at %d", i)
		}
	}
}

func TestRuntime_Bands(t *testing.T) {
	svc, _, userID := testService(t)

	rows, err := svc.runtime(context.Background(), userID)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no runtime bands")
	}
	// 95 -> 90-119, 120 -> 120-149, 210 -> 210-239. None hits the 240+ tail.
	labels := map[string]bool{}
	for _, r := range rows {
		labels[r.Label] = true
	}
	for _, want := range []string{"90-119", "120-149", "210-239"} {
		if !labels[want] {
			t.Errorf("missing runtime band %q; got %v", want, labels)
		}
	}
}

func TestPeople_SeparatesRoles(t *testing.T) {
	svc, _, userID := testService(t)

	out, err := svc.people(context.Background(), userID)
	if err != nil {
		t.Fatalf("people: %v", err)
	}

	directors := out["directors"].([]Bucket)
	actors := out["actors"].([]Bucket)
	studios := out["studios"].([]Bucket)

	// dir-a directed two of the three films, so it must rank first.
	if len(directors) == 0 {
		t.Fatal("no directors")
	}
	if directors[0].Count != 2 {
		t.Errorf("top director count = %d, want 2", directors[0].Count)
	}
	// The same person_slug must not leak across roles.
	for _, d := range directors {
		if d.Label == "Actor X" {
			t.Error("an actor appeared in the directors list")
		}
	}
	if len(actors) == 0 || actors[0].Count != 3 {
		t.Errorf("top actor = %v, want a count of 3", actors)
	}
	if len(studios) != 3 {
		t.Errorf("got %d studios, want 3", len(studios))
	}
}

func TestActivity_DailyMonthlyWeekday(t *testing.T) {
	svc, _, userID := testService(t)

	out, err := svc.activity(context.Background(), userID)
	if err != nil {
		t.Fatalf("activity: %v", err)
	}

	daily := out["daily"].([]DayCount)
	monthly := out["monthly"].([]DayCount)
	weekday := out["weekday"].([]DayCount)

	if len(daily) != 3 {
		t.Errorf("daily = %v, want 3 distinct dates", daily)
	}
	if len(daily) > 0 && daily[0].Date != "2024-03-04" {
		t.Errorf("daily[0] = %q, want 2024-03-04 (ascending)", daily[0].Date)
	}
	// Two entries in March, one in April.
	if len(monthly) != 2 {
		t.Fatalf("monthly = %v, want 2 months", monthly)
	}
	if monthly[0].Date != "2024-03" || monthly[0].Count != 2 {
		t.Errorf("monthly[0] = %+v, want 2024-03 with 2", monthly[0])
	}
	// All three dates are Mondays and Tuesdays; labels must be trimmed, not padded.
	for _, w := range weekday {
		if w.Date != "Monday" && w.Date != "Tuesday" {
			t.Errorf("unexpected weekday label %q (check the to_char trim)", w.Date)
		}
	}
}

// Compute must work for every declared category: this catches a query that only
// breaks when reached through the API.
func TestCompute_AllCategories(t *testing.T) {
	svc, _, userID := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, cat := range AllCategories {
		payload, err := svc.Compute(ctx, userID, cat)
		if err != nil {
			t.Errorf("Compute(%s): %v", cat, err)
			continue
		}
		if payload == nil {
			t.Errorf("Compute(%s) returned nil", cat)
		}
	}
}

func TestCompute_UnknownCategoryIsAnError(t *testing.T) {
	svc, _, userID := testService(t)
	if _, err := svc.Compute(context.Background(), userID, Category("nope")); err == nil {
		t.Fatal("want an error for an unknown category")
	}
}

// A user with no data must yield empty results, not an error: the API serves this
// state while a first sync is still running.
func TestCompute_EmptyUser(t *testing.T) {
	svc, pool, _ := testService(t)
	ctx := context.Background()

	var emptyID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (username) VALUES ('nobody') RETURNING id`).Scan(&emptyID); err != nil {
		t.Fatalf("create empty user: %v", err)
	}

	for _, cat := range AllCategories {
		if _, err := svc.Compute(ctx, emptyID, cat); err != nil {
			t.Errorf("Compute(%s) for an empty user: %v", cat, err)
		}
	}

	o, err := svc.overview(ctx, emptyID)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if o.FilmsLogged != 0 || o.AvgRating != nil {
		t.Errorf("empty overview = %+v, want zeros and a nil average", o)
	}
}
