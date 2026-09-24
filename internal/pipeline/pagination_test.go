package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/danielkwan-dev/memento/internal/letterboxd"
)

// Letterboxd's paginator over-reports. Measured on a real 2520-film profile: the
// paginator links page 36, then serves 403 for page 36 AND page 37 (past the
// end), while pages 1-35 return instantly with zero retries. Treating that as a
// fatal error threw away 35 good pages.
//
// So a failure on the LAST ADVERTISED page means "no more data", while a failure
// on any earlier page is still a real error. These tests pin both halves down.
type tailFailFetcher struct {
	*fakeFetcher
	// failPage returns an error for exactly this grid page.
	failPage int
}

func (f *tailFailFetcher) Get(ctx context.Context, url string) ([]byte, error) {
	if strings.Contains(url, "/films/page/") && !strings.Contains(url, "/diary/") {
		if pageOf(url) == f.failPage {
			return nil, letterboxd.ErrBlocked
		}
	}
	return f.fakeFetcher.Get(ctx, url)
}

func TestIndex_Tolerates403OnLastAdvertisedPage(t *testing.T) {
	base := newFakeFetcher()
	base.gridPages, base.diaryPages, base.filmsPer = 5, 1, 4
	// The paginator advertises 5 pages but the last one 403s, exactly as
	// Letterboxd behaves on a large profile.
	f := &tailFailFetcher{fakeFetcher: base, failPage: 5}
	s := newFakeStore()

	res, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	if err != nil {
		t.Fatalf("a 403 on the last advertised page must not fail the run: %v", err)
	}

	// Pages 1-4 of the grid survive: 4 pages x 4 films.
	if res.WatchEntries != 16 {
		t.Errorf("WatchEntries = %d, want 16 (pages 1-4 kept)", res.WatchEntries)
	}
	if len(s.watchEntries) != 16 {
		t.Errorf("persisted %d watch entries, want 16", len(s.watchEntries))
	}
}

// A failure in the middle of pagination is a genuine problem: silently dropping
// page 3 of 10 would store an incomplete profile as if it were complete.
func TestIndex_FailsOnMiddlePageFailure(t *testing.T) {
	base := newFakeFetcher()
	base.gridPages, base.diaryPages, base.filmsPer = 10, 1, 4
	f := &tailFailFetcher{fakeFetcher: base, failPage: 3}
	s := newFakeStore()

	_, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	if err == nil {
		t.Fatal("want an error when a middle page fails, got nil")
	}
	if !strings.Contains(err.Error(), "page 3") {
		t.Errorf("error should name the failing page, got: %v", err)
	}
	// Nothing may be persisted from a partial index.
	if len(s.watchEntries) != 0 {
		t.Errorf("persisted %d watch entries despite a mid-pagination failure",
			len(s.watchEntries))
	}
}

// Pages must be applied in order regardless of the order they arrive in, so a
// run is reproducible.
func TestIndex_PagesAppliedInOrder(t *testing.T) {
	f := newFakeFetcher()
	f.gridPages, f.diaryPages, f.filmsPer = 4, 1, 3
	s := newFakeStore()

	res, err := quietPipeline(f, s, testConfig()).
		Run(context.Background(), uuid.New(), "someone")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.WatchEntries != 12 {
		t.Fatalf("WatchEntries = %d, want 12", res.WatchEntries)
	}

	// renderGrid names films film-<(page-1)*perPage + i>, so ordered pages give
	// film-0, film-1, ... film-11.
	for i, e := range s.watchEntries {
		want := "film-" + itoa(i)
		if e.FilmSlug != want {
			t.Errorf("entry %d = %s, want %s (pages applied out of order)",
				i, e.FilmSlug, want)
			break
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
