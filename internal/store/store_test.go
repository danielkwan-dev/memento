package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/danielkwan-dev/memento/internal/letterboxd"
)

// These tests need a real Postgres, because the behaviour under test IS the SQL:
// the staleness diff, FOR UPDATE SKIP LOCKED, the active-job unique index,
// LISTEN/NOTIFY and the delete cascade. A mock would only assert that we send
// the strings we send.
//
// Start one with:
//
//	docker compose up -d postgres
//	MEMENTO_TEST_DATABASE_URL=postgres://memento:memento@localhost:5432/memento?sslmode=disable go test ./internal/store/
//
// Without that variable the package is skipped, so the default suite stays
// offline.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("MEMENTO_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set MEMENTO_TEST_DATABASE_URL to run store tests (docker compose up -d postgres)")
	}

	// Own schema, so a parallel package's TRUNCATE cannot wipe these rows.
	t.Setenv("MEMENTO_DB_SCHEMA", "test_store")

	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Each test starts from a clean slate. films cascades to its association
	// tables, users to watch/diary entries.
	truncate(t, st)
	return st
}

func truncate(t *testing.T, st *Store) {
	t.Helper()
	_, err := st.pool.Exec(context.Background(),
		`TRUNCATE jobs, user_stats, diary_entries, watch_entries, users, films CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func TestMigrate_IsIdempotent(t *testing.T) {
	st := testStore(t)
	// A second run must be a no-op, which is what makes it safe for both the api
	// and the worker to migrate on startup.
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func sampleFilm(slug string) *letterboxd.FilmDetails {
	return &letterboxd.FilmDetails{
		Slug:        slug,
		Title:       "Title " + slug,
		Year:        2019,
		RuntimeMin:  120,
		Genres:      []string{"Drama", "Thriller"},
		Themes:      []string{"Some Theme"},
		Directors:   []letterboxd.Person{{Slug: "dir-a", Name: "Dir A"}},
		Cast:        []letterboxd.Person{{Slug: "act-a", Name: "Act A", BillingOrder: 1}},
		Studios:     []string{"Studio A"},
		Countries:   []string{"South Korea"},
		Languages:   []string{"Korean"},
		AvgRating:   4.2,
		RatingCount: 1000,
	}
}

func TestUpsertFilmDetails_AndAssociations(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if err := st.UpsertFilmDetails(ctx, sampleFilm("film-a")); err != nil {
		t.Fatalf("UpsertFilmDetails: %v", err)
	}

	var title string
	var year, runtime int
	if err := st.pool.QueryRow(ctx,
		`SELECT title, year, runtime_min FROM films WHERE slug = 'film-a'`).
		Scan(&title, &year, &runtime); err != nil {
		t.Fatalf("read film: %v", err)
	}
	if year != 2019 || runtime != 120 {
		t.Errorf("year/runtime = %d/%d, want 2019/120", year, runtime)
	}

	for _, tc := range []struct {
		table string
		want  int
	}{
		{"film_genres", 2}, {"film_themes", 1}, {"film_studios", 1},
		{"film_countries", 1}, {"film_languages", 1},
		{"film_people", 2}, // one director + one cast member
	} {
		var n int
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM `+tc.table+` WHERE film_slug = 'film-a'`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tc.table, err)
		}
		if n != tc.want {
			t.Errorf("%s has %d rows, want %d", tc.table, n, tc.want)
		}
	}
}

// Re-scraping must replace associations, not accumulate them. A film losing a
// genre upstream has to lose it here too.
func TestUpsertFilmDetails_ReplacesAssociations(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if err := st.UpsertFilmDetails(ctx, sampleFilm("film-a")); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	reduced := sampleFilm("film-a")
	reduced.Genres = []string{"Comedy"} // was Drama + Thriller
	if err := st.UpsertFilmDetails(ctx, reduced); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	rows, err := st.pool.Query(ctx,
		`SELECT genre FROM film_genres WHERE film_slug = 'film-a' ORDER BY genre`)
	if err != nil {
		t.Fatalf("query genres: %v", err)
	}
	defer rows.Close()
	var genres []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			t.Fatal(err)
		}
		genres = append(genres, g)
	}
	if len(genres) != 1 || genres[0] != "Comedy" {
		t.Errorf("genres = %v, want [Comedy]", genres)
	}
}

// The cache-miss diff is the core of the global film cache.
func TestClassifySlugs(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	// Fresh: details and stats both current.
	if err := st.UpsertFilmDetails(ctx, sampleFilm("fresh")); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertFilmStats(ctx, &letterboxd.FilmStats{
		Slug: "fresh", WatchCount: 10, LikeCount: 2}); err != nil {
		t.Fatal(err)
	}

	// Details current but stats never fetched.
	if err := st.UpsertFilmDetails(ctx, sampleFilm("no-stats")); err != nil {
		t.Fatal(err)
	}

	// Details deliberately aged past the TTL.
	if err := st.UpsertFilmDetails(ctx, sampleFilm("stale")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE films SET details_fetched_at = now() - interval '200 days',
		                  stats_fetched_at   = now() - interval '200 days'
		 WHERE slug = 'stale'`); err != nil {
		t.Fatal(err)
	}

	split, err := st.ClassifySlugs(ctx,
		[]string{"fresh", "no-stats", "stale", "brand-new"},
		90*24*time.Hour, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("ClassifySlugs: %v", err)
	}

	if split.CacheHits != 1 {
		t.Errorf("CacheHits = %d, want 1 (fresh)", split.CacheHits)
	}
	if !contains(split.NeedDetails, "brand-new") {
		t.Errorf("NeedDetails %v must include brand-new", split.NeedDetails)
	}
	if !contains(split.NeedDetails, "stale") {
		t.Errorf("NeedDetails %v must include stale", split.NeedDetails)
	}
	if !contains(split.NeedStats, "no-stats") {
		t.Errorf("NeedStats %v must include no-stats", split.NeedStats)
	}
	// A film needing details must not also be queued for a stats-only fetch: the
	// details fetch covers it.
	if contains(split.NeedStats, "stale") {
		t.Error("stale appears in both NeedDetails and NeedStats")
	}
}

// A stats refresh must not disturb metadata or the details timestamp.
func TestUpsertFilmStats_DoesNotTouchDetails(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if err := st.UpsertFilmDetails(ctx, sampleFilm("film-a")); err != nil {
		t.Fatal(err)
	}
	var before time.Time
	if err := st.pool.QueryRow(ctx,
		`SELECT details_fetched_at FROM films WHERE slug='film-a'`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	if err := st.UpsertFilmStats(ctx, &letterboxd.FilmStats{
		Slug: "film-a", WatchCount: 500, LikeCount: 50}); err != nil {
		t.Fatalf("UpsertFilmStats: %v", err)
	}

	var after time.Time
	var title string
	var watch int64
	if err := st.pool.QueryRow(ctx,
		`SELECT details_fetched_at, title, watch_count FROM films WHERE slug='film-a'`).
		Scan(&after, &title, &watch); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Error("details_fetched_at changed during a stats-only refresh")
	}
	if title != "Title film-a" {
		t.Errorf("title = %q, want it preserved", title)
	}
	if watch != 500 {
		t.Errorf("watch_count = %d, want 500", watch)
	}
}

func TestEnqueueJob_IsIdempotentPerUser(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	first, err := st.EnqueueJob(ctx, "alice", KindScrape)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if first.Phase != PhaseWaking {
		t.Errorf("phase = %s, want %s so the UI can show the cold start", first.Phase, PhaseWaking)
	}

	// The unique partial index must reject a second active job for this user.
	if _, err := st.EnqueueJob(ctx, "alice", KindScrape); !errors.Is(err, ErrJobInFlight) {
		t.Fatalf("second enqueue error = %v, want ErrJobInFlight", err)
	}

	// Username matching is case-insensitive, matching Letterboxd's URLs.
	if _, err := st.EnqueueJob(ctx, "ALICE", KindScrape); !errors.Is(err, ErrJobInFlight) {
		t.Errorf("mixed-case enqueue error = %v, want ErrJobInFlight", err)
	}

	// A different user is unaffected.
	if _, err := st.EnqueueJob(ctx, "bob", KindScrape); err != nil {
		t.Errorf("enqueue for another user failed: %v", err)
	}

	// Once the job is terminal, a new one is allowed.
	if err := st.FinishJob(ctx, first.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueJob(ctx, "alice", KindScrape); err != nil {
		t.Errorf("enqueue after completion failed: %v", err)
	}
}

func TestClaimJob_SkipsLockedAndOrdersByAge(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	a, err := st.EnqueueJob(ctx, "alice", KindScrape)
	if err != nil {
		t.Fatal(err)
	}
	// Force a distinct created_at so ordering is deterministic.
	if _, err := st.pool.Exec(ctx,
		`UPDATE jobs SET created_at = now() - interval '1 minute' WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	b, err := st.EnqueueJob(ctx, "bob", KindScrape)
	if err != nil {
		t.Fatal(err)
	}

	// Oldest first.
	got, err := st.ClaimJob(ctx, "worker-1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got == nil || got.ID != a.ID {
		t.Fatalf("claimed %v, want the older job %v", got, a.ID)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %s, want running", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}

	// A second worker must get the other job, never the same one.
	got2, err := st.ClaimJob(ctx, "worker-2")
	if err != nil {
		t.Fatal(err)
	}
	if got2 == nil || got2.ID != b.ID {
		t.Fatalf("second claim = %v, want %v", got2, b.ID)
	}

	// Queue drained.
	if got3, err := st.ClaimJob(ctx, "worker-3"); err != nil || got3 != nil {
		t.Errorf("third claim = %v (err %v), want nil", got3, err)
	}
}

func TestReapStaleJobs(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	job, err := st.EnqueueJob(ctx, "alice", KindScrape)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimJob(ctx, "dead-worker"); err != nil {
		t.Fatal(err)
	}
	// Simulate a worker that died without updating progress.
	if _, err := st.pool.Exec(ctx,
		`UPDATE jobs SET locked_at = now() - interval '30 minutes' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}

	requeued, failed, err := st.ReapStaleJobs(ctx, 5*time.Minute, 3)
	if err != nil {
		t.Fatalf("ReapStaleJobs: %v", err)
	}
	if requeued != 1 || failed != 0 {
		t.Errorf("requeued/failed = %d/%d, want 1/0", requeued, failed)
	}

	after, err := st.JobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after == nil {
		t.Fatal("job not found; is a worker polling the test database?")
	}
	if after.Status != StatusQueued {
		t.Errorf("status = %s, want queued", after.Status)
	}
	// Requeued jobs return to 'waking': the next worker may be cold too.
	if after.Phase != PhaseWaking {
		t.Errorf("phase = %s, want %s", after.Phase, PhaseWaking)
	}

	// With attempts exhausted it must fail rather than loop forever.
	if _, err := st.pool.Exec(ctx,
		`UPDATE jobs SET status='running', attempts = 3,
		                 locked_at = now() - interval '30 minutes' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	requeued, failed, err = st.ReapStaleJobs(ctx, 5*time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 0 || failed != 1 {
		t.Errorf("requeued/failed = %d/%d, want 0/1", requeued, failed)
	}
}

// Progress must be both durable (so a reconnecting client recovers state) and
// published (so a connected client sees it live).
func TestUpdateProgress_PersistsAndNotifies(t *testing.T) {
	st := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	job, err := st.EnqueueJob(ctx, "alice", KindScrape)
	if err != nil {
		t.Fatal(err)
	}

	notifications, err := st.Listen(ctx, JobChannel(job.ID))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	if err := st.UpdateProgress(ctx, job.ID, PhaseHydrate, 7, 20, 3); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}

	select {
	case payload := <-notifications:
		if payload == "" {
			t.Error("empty notification payload")
		}
		t.Logf("notification: %s", payload)
	case <-time.After(5 * time.Second):
		t.Fatal("no NOTIFY received within 5s")
	}

	reloaded, err := st.JobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded == nil {
		t.Fatal("job not found; is a worker polling the test database?")
	}
	if reloaded.FilmsDone != 7 || reloaded.FilmsTotal != 20 || reloaded.CacheHits != 3 {
		t.Errorf("persisted progress = %d/%d hits=%d, want 7/20 hits=3",
			reloaded.FilmsDone, reloaded.FilmsTotal, reloaded.CacheHits)
	}
	if reloaded.Phase != PhaseHydrate {
		t.Errorf("phase = %s, want hydrate", reloaded.Phase)
	}
}

func TestReplaceEntries_AndDeleteUserCascade(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	userID, err := st.UpsertUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureFilmStubs(ctx,
		[]string{"film-a", "film-b"},
		[]string{"Film A", "Film B"},
		[]int32{2019, 2020}); err != nil {
		t.Fatalf("EnsureFilmStubs: %v", err)
	}

	r := 4.0
	if err := st.ReplaceWatchEntries(ctx, userID, []letterboxd.WatchEntry{
		{FilmSlug: "film-a", Rating: &r, Liked: true},
		{FilmSlug: "film-b"},
	}); err != nil {
		t.Fatalf("ReplaceWatchEntries: %v", err)
	}
	if err := st.ReplaceDiaryEntries(ctx, userID, []letterboxd.DiaryEntry{
		{FilmSlug: "film-a", WatchedOn: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC), Rating: &r},
		{FilmSlug: "film-a", WatchedOn: time.Date(2024, 5, 9, 0, 0, 0, 0, time.UTC), Rewatch: true},
	}); err != nil {
		t.Fatalf("ReplaceDiaryEntries: %v", err)
	}

	// A film can legitimately appear twice in the diary on different dates.
	var diaryCount int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM diary_entries WHERE user_id = $1`, userID).Scan(&diaryCount); err != nil {
		t.Fatal(err)
	}
	if diaryCount != 2 {
		t.Errorf("diary rows = %d, want 2 (rewatch on a second date)", diaryCount)
	}

	// Replacing must be a full swap, not an append.
	if err := st.ReplaceWatchEntries(ctx, userID, []letterboxd.WatchEntry{
		{FilmSlug: "film-a", Rating: &r},
	}); err != nil {
		t.Fatal(err)
	}
	var watchCount int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM watch_entries WHERE user_id = $1`, userID).Scan(&watchCount); err != nil {
		t.Fatal(err)
	}
	if watchCount != 1 {
		t.Errorf("watch rows = %d, want 1 after replacement", watchCount)
	}

	// Deleting the user must clear personal rows and leave the film cache alone:
	// films hold only public facts, shared across users.
	deleted, err := st.DeleteUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("DeleteUser reported no rows removed")
	}

	for _, table := range []string{"watch_entries", "diary_entries"} {
		var n int
		if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s still has %d rows after user deletion", table, n)
		}
	}
	var films int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM films`).Scan(&films); err != nil {
		t.Fatal(err)
	}
	if films != 2 {
		t.Errorf("films = %d, want 2 preserved (the global cache is not personal data)", films)
	}
}

// Ratings are NUMERIC(2,1) with a range check, so the database itself rejects
// impossible values instead of trusting the parser.
func TestRatingConstraint(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	userID, err := st.UpsertUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureFilmStubs(ctx, []string{"film-a"}, []string{"Film A"}, []int32{2019}); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []float64{0, 5.5, -1} {
		_, err := st.pool.Exec(ctx,
			`INSERT INTO watch_entries (user_id, film_slug, rating) VALUES ($1, 'film-a', $2)`,
			userID, bad)
		if err == nil {
			t.Errorf("rating %v was accepted; the CHECK constraint should reject it", bad)
			_, _ = st.pool.Exec(ctx, `DELETE FROM watch_entries WHERE user_id = $1`, userID)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// The blocked flag must survive a round trip, because the UI branches on it to
// offer the export upload instead of a generic failure.
func TestFinishJob_RecordsBlocked(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	blockedJob, err := st.EnqueueJob(ctx, "alice", KindScrape)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishJob(ctx, blockedJob.ID, "letterboxd would not serve this host", true); err != nil {
		t.Fatalf("FinishJob: %v", err)
	}
	got, err := st.JobByID(ctx, blockedJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Blocked {
		t.Error("Blocked = false, want true")
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %s, want failed", got.Status)
	}

	// An ordinary failure must NOT be marked blocked, or the UI would push the
	// export upload at users whose problem is something else entirely.
	plainJob, err := st.EnqueueJob(ctx, "bob", KindScrape)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishJob(ctx, plainJob.ID, "some other failure", false); err != nil {
		t.Fatal(err)
	}
	got, err = st.JobByID(ctx, plainJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Blocked {
		t.Error("Blocked = true for a non-blocking failure")
	}

	// A success is never blocked.
	okJob, err := st.EnqueueJob(ctx, "carol", KindScrape)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishJob(ctx, okJob.ID, "", false); err != nil {
		t.Fatal(err)
	}
	got, err = st.JobByID(ctx, okJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Blocked || got.Status != StatusSucceeded {
		t.Errorf("succeeded job = %s blocked=%v", got.Status, got.Blocked)
	}
}
