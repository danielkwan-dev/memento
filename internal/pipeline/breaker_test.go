package pipeline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// When the upstream is blocking, every film burns its full retry budget (6
// attempts backing off to 60s). Measured against the live site, a 242-film job
// spent 25 minutes grinding through 59 failures before the ratio check in Run
// ever looked at them -- the job could not fail fast even though it was doomed
// after the first handful.
//
// The breaker fixes that, and these tests pin both halves: it must trip promptly
// on a doomed run, and it must never fail a run that would otherwise succeed.
func TestBreaker_TripsEarlyWhenMostFilmsFail(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.filmsPer = 1, 200
	// Fail every film: the worst case, a fully blocking upstream.
	for i := 0; i < 200; i++ {
		f.failSlugs[fmt.Sprintf("film-%d", i)] = true
	}
	// Enough latency that grinding through all 200 would be obviously slower.
	f.delay = 5 * time.Millisecond
	s := newFakeStore()

	start := time.Now()
	_, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error when every film fails")
	}
	if !errors.Is(err, ErrTooManyFailures) {
		t.Errorf("error should wrap ErrTooManyFailures, got: %v", err)
	}
	// The message must point at the likely cause rather than being opaque.
	if !contains(err.Error(), "rate limiting") {
		t.Errorf("error should mention rate limiting, got: %v", err)
	}

	// The breaker must stop well short of attempting all 200 films.
	attempted := f.hitCount("/film/")
	if attempted > 150 {
		t.Errorf("attempted %d film requests; the breaker should have tripped much earlier", attempted)
	}
	t.Logf("tripped after ~%d requests in %s", attempted, elapsed.Round(time.Millisecond))

	// Nothing may be persisted from a failed run.
	if len(s.watchEntries) != 0 {
		t.Errorf("persisted %d watch entries despite failure", len(s.watchEntries))
	}
}

// The breaker must not be trigger-happy: a failure rate under the threshold has
// to complete normally.
func TestBreaker_DoesNotTripBelowThreshold(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.filmsPer = 1, 100
	// 10 of 100 grid films fail. With the diary's films added the overall rate is
	// lower still, comfortably under the 20% threshold.
	for i := 0; i < 10; i++ {
		f.failSlugs[fmt.Sprintf("film-%d", i)] = true
	}
	s := newFakeStore()

	res, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	if err != nil {
		t.Fatalf("a 10%% failure rate must not trip the breaker: %v", err)
	}
	if res.FilmFailures != 10 {
		t.Errorf("FilmFailures = %d, want 10", res.FilmFailures)
	}
	// The run completed, so entries are persisted.
	if len(s.watchEntries) != 100 {
		t.Errorf("persisted %d watch entries, want 100", len(s.watchEntries))
	}
}

// A small profile must not trip on a couple of unlucky films, which is why the
// breaker requires a minimum sample.
func TestBreaker_IgnoresSmallSamples(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.filmsPer = 1, 8
	// Half the (tiny) grid fails -- far above the ratio, but below the sample floor.
	for i := 0; i < 4; i++ {
		f.failSlugs[fmt.Sprintf("film-%d", i)] = true
	}
	s := newFakeStore()

	_, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	// Run's own ratio check still fails this job -- correctly, since the rate is
	// genuinely bad -- but it must NOT be the breaker that did it, because the
	// sample was too small to judge mid-flight.
	if err != nil && errors.Is(err, ErrTooManyFailures) {
		t.Errorf("the breaker tripped on only %d films; it must wait for %d samples",
			f.filmsPer, breakerMinSamples)
	}
}

func TestBreakerTripped(t *testing.T) {
	p := quietPipeline(newFakeFetcher(), newFakeStore(), testConfig())

	// The threshold is 20%, and the breaker allows 2x slack while in flight, so it
	// fires above 40% rather than above 20%.
	cases := []struct {
		name                  string
		done, failures, total int
		want                  bool
	}{
		{"below sample floor", 10, 10, 200, false},
		{"small total", 50, 50, 10, false},
		{"at the job threshold", 100, 20, 200, false},
		{"clustered early failures", 100, 35, 200, false},
		{"just over the breaker limit", 100, 41, 200, true},
		{"everything failing", 50, 50, 200, true},
		{"healthy", 100, 5, 200, false},
	}
	for _, c := range cases {
		if got := p.breakerTripped(c.done, c.failures, c.total); got != c.want {
			t.Errorf("%s: breakerTripped(%d, %d, %d) = %v, want %v",
				c.name, c.done, c.failures, c.total, got, c.want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
