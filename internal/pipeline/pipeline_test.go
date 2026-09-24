package pipeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danielkwan-dev/memento/internal/letterboxd"
	"github.com/danielkwan-dev/memento/internal/store"
)

// --- fakes -------------------------------------------------------------------

// fakeFetcher serves synthesized pages. Generating them beats saving thousands
// of fixtures: these tests exercise concurrency, caching and error handling, not
// HTML parsing (which the letterboxd package's fixture tests cover).
type fakeFetcher struct {
	mu sync.Mutex
	// hits records how many times each URL was requested, so a test can assert
	// the cache actually prevented fetches.
	hits map[string]int
	// failSlugs always error, simulating deleted or blocked films.
	failSlugs map[string]bool
	// gridPages and diaryPages control pagination.
	gridPages  int
	diaryPages int
	filmsPer   int
	// delay simulates network latency so concurrency limits are observable.
	delay time.Duration
	// inFlight tracks peak concurrency to verify the semaphores.
	inFlight     atomic.Int64
	peakInFlight atomic.Int64
	noDiary      bool
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		hits:       map[string]int{},
		failSlugs:  map[string]bool{},
		gridPages:  1,
		diaryPages: 1,
		filmsPer:   3,
	}
}

func (f *fakeFetcher) Get(ctx context.Context, url string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cur := f.inFlight.Add(1)
	for {
		peak := f.peakInFlight.Load()
		if cur <= peak || f.peakInFlight.CompareAndSwap(peak, cur) {
			break
		}
	}
	defer f.inFlight.Add(-1)

	f.mu.Lock()
	f.hits[url]++
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}

	switch {
	case strings.Contains(url, "/csi/film/"):
		slug := between(url, "/csi/film/", "/stats/")
		if f.failSlugs[slug] {
			return nil, fmt.Errorf("simulated stats failure for %s", slug)
		}
		return []byte(renderStats(slug)), nil

	case strings.Contains(url, "/film/"):
		slug := strings.Trim(between(url, "/film/", ""), "/")
		if f.failSlugs[slug] {
			return nil, letterboxd.ErrNotFound
		}
		return []byte(renderFilm(slug)), nil

	case strings.Contains(url, "/films/diary/"):
		if f.noDiary {
			return nil, letterboxd.ErrNotFound
		}
		return []byte(renderDiary(pageOf(url), f.diaryPages, f.filmsPer)), nil

	case strings.Contains(url, "/films/"):
		return []byte(renderGrid(pageOf(url), f.gridPages, f.filmsPer)), nil
	}
	return nil, fmt.Errorf("unexpected url %s", url)
}

func (f *fakeFetcher) hitCount(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int
	for url, c := range f.hits {
		if strings.Contains(url, substr) {
			n += c
		}
	}
	return n
}

// detailHits counts only film DETAIL page requests. Plain substring matching
// would also catch "/csi/film/<slug>/stats/", which contains "/film/<slug>/".
func (f *fakeFetcher) detailHits(slug string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[letterboxd.BaseURL+"/film/"+slug+"/"]
}

// fakeStore records writes and can pretend films are already cached.
type fakeStore struct {
	mu sync.Mutex

	cachedDetails map[string]bool
	cachedStats   map[string]bool

	upsertedDetails []string
	upsertedStats   []string
	watchEntries    []letterboxd.WatchEntry
	diaryEntries    []letterboxd.DiaryEntry
	progress        []store.JobPhase
	syncedUser      int64
	stubCount       int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		cachedDetails: map[string]bool{},
		cachedStats:   map[string]bool{},
	}
}

func (s *fakeStore) UpsertUser(context.Context, string) (int64, error) { return 42, nil }

func (s *fakeStore) ClassifySlugs(_ context.Context, slugs []string, _, _ time.Duration) (*store.StaleSplit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &store.StaleSplit{}
	for _, slug := range slugs {
		switch {
		case !s.cachedDetails[slug]:
			out.NeedDetails = append(out.NeedDetails, slug)
		case !s.cachedStats[slug]:
			out.NeedStats = append(out.NeedStats, slug)
		default:
			out.CacheHits++
		}
	}
	return out, nil
}

func (s *fakeStore) UpsertFilmDetails(_ context.Context, d *letterboxd.FilmDetails) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertedDetails = append(s.upsertedDetails, d.Slug)
	return nil
}

func (s *fakeStore) UpsertFilmStats(_ context.Context, st *letterboxd.FilmStats) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertedStats = append(s.upsertedStats, st.Slug)
	return nil
}

func (s *fakeStore) EnsureFilmStubs(_ context.Context, slugs, _ []string, _ []int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stubCount = len(slugs)
	return nil
}

func (s *fakeStore) ReplaceWatchEntries(_ context.Context, _ int64, e []letterboxd.WatchEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watchEntries = e
	return nil
}

func (s *fakeStore) ReplaceDiaryEntries(_ context.Context, _ int64, e []letterboxd.DiaryEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diaryEntries = e
	return nil
}

func (s *fakeStore) MarkUserSynced(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncedUser = id
	return nil
}

func (s *fakeStore) UpdateProgress(_ context.Context, _ uuid.UUID, phase store.JobPhase, _, _, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Collapse repeated ticks of the same phase.
	if n := len(s.progress); n == 0 || s.progress[n-1] != phase {
		s.progress = append(s.progress, phase)
	}
	return nil
}

// --- helpers -----------------------------------------------------------------

func quietPipeline(f letterboxd.Fetcher, s Store, cfg Config) *Pipeline {
	return New(f, s, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testConfig() Config {
	c := DefaultConfig()
	c.IndexConcurrency = 2
	c.DetailConcurrency = 4
	return c
}

// --- tests -------------------------------------------------------------------

func TestRun_HappyPath(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.diaryPages, f.filmsPer = 2, 1, 3
	s := newFakeStore()

	res, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 2 grid pages x 3 films = 6 unique films.
	if res.WatchEntries != 6 {
		t.Errorf("WatchEntries = %d, want 6", res.WatchEntries)
	}
	// 1 diary page x 3 films, in their own slug namespace.
	if res.DiaryEntries != 3 {
		t.Errorf("DiaryEntries = %d, want 3", res.DiaryEntries)
	}
	// Hydration covers the union of grid and diary films.
	if res.FilmsFetched != 9 {
		t.Errorf("FilmsFetched = %d, want 9 (6 grid + 3 diary)", res.FilmsFetched)
	}
	if res.CacheHits != 0 {
		t.Errorf("CacheHits = %d, want 0 on a cold cache", res.CacheHits)
	}
	if s.syncedUser != 42 {
		t.Errorf("user not marked synced (got %d)", s.syncedUser)
	}

	// Phases must be reported in order so the SSE stream is coherent.
	want := []store.JobPhase{
		store.PhaseResolve, store.PhaseIndex, store.PhaseHydrate, store.PhasePersist,
	}
	if len(s.progress) != len(want) {
		t.Fatalf("phases = %v, want %v", s.progress, want)
	}
	for i := range want {
		if s.progress[i] != want[i] {
			t.Errorf("phase[%d] = %s, want %s", i, s.progress[i], want[i])
		}
	}
}

// The point of the global film cache: already-cached films must not be refetched.
func TestRun_CacheHitsSkipFetching(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.filmsPer = 1, 4
	s := newFakeStore()

	// Pretend every film but one is fully cached.
	for i := 0; i < 4; i++ {
		slug := fmt.Sprintf("film-%d", i)
		if slug == "film-2" {
			continue
		}
		s.cachedDetails[slug] = true
		s.cachedStats[slug] = true
	}

	res, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.CacheHits != 3 {
		t.Errorf("CacheHits = %d, want 3", res.CacheHits)
	}
	// film-2 plus the diary films, which share filmsPer and are never cached.
	wantFetched := 1 + f.filmsPer
	if res.FilmsFetched != wantFetched {
		t.Errorf("FilmsFetched = %d, want %d (film-2 + %d diary films)",
			res.FilmsFetched, wantFetched, f.filmsPer)
	}
	// The uncached grid film is fetched exactly once.
	if got := f.detailHits("film-2"); got != 1 {
		t.Errorf("film-2 detail fetched %d times, want 1", got)
	}
	// Cached films must never be fetched at all: this is the whole point of the
	// global film cache.
	for _, cached := range []string{"film-0", "film-1", "film-3"} {
		if got := f.detailHits(cached); got != 0 {
			t.Errorf("cached %s was fetched %d times, want 0", cached, got)
		}
	}
}

// One deleted film must not abandon the whole scrape.
func TestRun_TolerateSomeFilmFailures(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.filmsPer = 1, 10
	f.failSlugs["film-3"] = true
	s := newFakeStore()

	res, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	if err != nil {
		t.Fatalf("Run should tolerate a single film failure, got: %v", err)
	}
	if res.FilmFailures != 1 {
		t.Errorf("FilmFailures = %d, want 1", res.FilmFailures)
	}
	// 10 grid films minus the failing one, plus the diary's films.
	wantFetched := (f.filmsPer - 1) + f.filmsPer
	if res.FilmsFetched != wantFetched {
		t.Errorf("FilmsFetched = %d, want %d", res.FilmsFetched, wantFetched)
	}
	// The user's entries are still stored, including the failed film's stub row.
	if len(s.watchEntries) != 10 {
		t.Errorf("stored %d watch entries, want 10", len(s.watchEntries))
	}
}

// Widespread failure is systemic and must fail loudly rather than store a
// gutted profile.
func TestRun_FailWhenTooManyFilmsFail(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.filmsPer = 1, 10
	for i := 0; i < 6; i++ {
		f.failSlugs[fmt.Sprintf("film-%d", i)] = true
	}
	s := newFakeStore()

	_, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	if err == nil {
		t.Fatal("want an error when most films fail, got nil")
	}
	if !strings.Contains(err.Error(), "threshold") {
		t.Errorf("error should name the threshold, got: %v", err)
	}
	// Nothing may be persisted for a failed run.
	if len(s.watchEntries) != 0 {
		t.Errorf("persisted %d watch entries despite failure", len(s.watchEntries))
	}
}

func TestRun_RespectsDetailConcurrencyLimit(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.filmsPer = 1, 30
	f.delay = 10 * time.Millisecond
	s := newFakeStore()

	cfg := testConfig()
	cfg.DetailConcurrency = 4

	if _, err := quietPipeline(f, s, cfg).
		Run(context.Background(), uuid.New(), "someone"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Index and hydrate phases don't overlap, so the peak must respect the
	// larger of the two limits and never exceed it.
	if peak := f.peakInFlight.Load(); peak > int64(cfg.DetailConcurrency) {
		t.Errorf("peak concurrency %d exceeded limit %d", peak, cfg.DetailConcurrency)
	}
}

func TestRun_CancellationStopsWork(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.filmsPer = 1, 50
	f.delay = 50 * time.Millisecond
	s := newFakeStore()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	_, err := quietPipeline(f, s, testConfig()).Run(ctx, uuid.New(), "someone")
	if err == nil {
		t.Fatal("want a cancellation error, got nil")
	}
	// Work must stop promptly rather than draining all 50 films.
	if got := f.hitCount("/film/"); got > 30 {
		t.Errorf("fetched %d films after cancellation; expected work to stop early", got)
	}
}

// A user with no diary is normal: non-temporal charts still work.
func TestRun_MissingDiaryIsNotFatal(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.filmsPer = 1, 3
	f.noDiary = true
	s := newFakeStore()

	res, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	if err != nil {
		t.Fatalf("a missing diary must not fail the run: %v", err)
	}
	if res.DiaryEntries != 0 {
		t.Errorf("DiaryEntries = %d, want 0", res.DiaryEntries)
	}
	if res.WatchEntries != 3 {
		t.Errorf("WatchEntries = %d, want 3", res.WatchEntries)
	}
}

func TestRun_EmptyProfileIsAnError(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.filmsPer = 1, 0
	f.noDiary = true
	s := newFakeStore()

	_, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "ghost")
	if err == nil {
		t.Fatal("want an error for a profile with no films")
	}
	if !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "empty") {
		t.Errorf("error should explain the empty/private case, got: %v", err)
	}
}

// Films appearing only in the diary must still be hydrated and stubbed.
func TestRun_DiaryOnlyFilmsAreIncluded(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.diaryPages, f.filmsPer = 1, 1, 2
	s := newFakeStore()

	res, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// renderDiary uses a distinct slug namespace, so the diary adds films.
	if s.stubCount <= res.WatchEntries {
		t.Errorf("stubCount = %d, want more than the %d grid films",
			s.stubCount, res.WatchEntries)
	}
}

// --- page synthesis ----------------------------------------------------------

func renderGrid(page, lastPage, perPage int) string {
	var b strings.Builder
	b.WriteString(`<html><body><ul class="poster-list">`)
	for i := 0; i < perPage; i++ {
		slug := fmt.Sprintf("film-%d", (page-1)*perPage+i)
		fmt.Fprintf(&b, `<li class="griditem">
			<div class="react-component" data-item-slug="%s" data-item-name="Film %s (2020)"></div>
			<p class="poster-viewingdata"><span class="rating rated-8">x</span>
			<span class="like liked-micro"></span></p></li>`, slug, slug)
	}
	b.WriteString(`</ul>`)
	b.WriteString(paginator(lastPage, "/someone/films"))
	b.WriteString(`</body></html>`)
	return b.String()
}

func renderDiary(page, lastPage, perPage int) string {
	var b strings.Builder
	b.WriteString(`<html><body><table>`)
	for i := 0; i < perPage; i++ {
		slug := fmt.Sprintf("diary-film-%d", (page-1)*perPage+i)
		fmt.Fprintf(&b, `<tr class="diary-entry-row">
			<td class="col-daydate"><a class="daydate" href="/someone/diary/films/for/2024/03/%02d/">%d</a></td>
			<td class="col-production"><div class="react-component" data-item-slug="%s" data-item-name="Diary Film %s (2021)"></div></td>
			<td class="col-releaseyear"><span>2021</span></td>
			<td class="col-rating"><input class="rateit-field" type="range" min="0" max="10" value="7"/></td>
			<td class="col-rewatch icon-status-off"></td></tr>`,
			i+1, i+1, slug, slug)
	}
	b.WriteString(`</table>`)
	b.WriteString(paginator(lastPage, "/someone/films/diary"))
	b.WriteString(`</body></html>`)
	return b.String()
}

func paginator(lastPage int, base string) string {
	if lastPage <= 1 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<div class="paginate-pages"><ul>`)
	for p := 2; p <= lastPage; p++ {
		fmt.Fprintf(&b, `<li class="paginate-page"><a href="%s/page/%d/">%d</a></li>`, base, p, p)
	}
	b.WriteString(`</ul></div>`)
	return b.String()
}

func renderFilm(slug string) string {
	return fmt.Sprintf(`<html><head>
		<meta property="og:title" content="Film %s (2020)">
		<script type="application/ld+json">{"name":"Film %s","duration":"PT1H50M",
			"genre":["Drama"],"countryOfOrigin":[{"name":"USA"}],
			"productionCompany":[{"name":"Studio X"}],
			"director":[{"name":"Dir One"}],"actor":[{"name":"Actor One"}],
			"aggregateRating":{"ratingValue":3.8,"ratingCount":1000}}</script>
		</head><body>
		<a href="/director/dir-one/">Dir One</a>
		<div id="tab-cast"><a href="/actor/actor-one/">Actor One</a></div>
		<a href="/films/genre/drama/">Drama</a>
		<a href="/films/language/english/">English</a>
		<a href="/films/theme/some-theme/by/best-match/">Some Theme</a>
		</body></html>`, slug, slug)
}

func renderStats(slug string) string {
	return fmt.Sprintf(`<div class="production-statistic-list">
		<div class="production-statistic -watches" aria-label="Watched by 12,345&nbsp;members"></div>
		<div class="production-statistic -likes" aria-label="Liked by 2,345&nbsp;members"></div>
		</div><!-- %s -->`, slug)
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	if end == "" {
		return rest
	}
	j := strings.Index(rest, end)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

func pageOf(url string) int {
	if i := strings.Index(url, "/page/"); i >= 0 {
		var n int
		fmt.Sscanf(url[i+6:], "%d", &n)
		if n > 0 {
			return n
		}
	}
	return 1
}
