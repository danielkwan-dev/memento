package letterboxd

import (
	"testing"
	"time"
)

func TestParseFilmsGrid(t *testing.T) {
	got, err := ParseFilmsGrid(readFixture(t, "films_grid_page1.html"))
	if err != nil {
		t.Fatalf("ParseFilmsGrid: %v", err)
	}
	// A full grid page holds 72 posters.
	if len(got) != 72 {
		t.Errorf("len = %d, want 72", len(got))
	}

	var rated, liked int
	for i, e := range got {
		if e.FilmSlug == "" {
			t.Errorf("entry %d has empty slug", i)
		}
		if e.Title == "" {
			t.Errorf("entry %d (%s) has empty title", i, e.FilmSlug)
		}
		// stripYear must remove the "(2019)" suffix from the display name.
		if len(e.Title) > 0 && e.Title[len(e.Title)-1] == ')' {
			t.Errorf("entry %d title still carries a year: %q", i, e.Title)
		}
		if e.Rating != nil {
			rated++
			if *e.Rating < 0.5 || *e.Rating > 5.0 {
				t.Errorf("entry %d rating %v out of range", i, *e.Rating)
			}
			// Ratings must land on exact half-star steps.
			if v := *e.Rating * 2; v != float64(int(v)) {
				t.Errorf("entry %d rating %v is not a half-star step", i, *e.Rating)
			}
		}
		if e.Liked {
			liked++
		}
	}
	if rated == 0 {
		t.Error("no ratings parsed; the rating selector is probably stale")
	}
	t.Logf("72 entries: %d rated, %d liked; first=%+v", rated, liked, got[0])
}

func TestParseDiaryPage(t *testing.T) {
	got, err := ParseDiaryPage(readFixture(t, "diary_page1.html"))
	if err != nil {
		t.Fatalf("ParseDiaryPage: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("len = %d, want 50", len(got))
	}

	var rated, rewatches int
	for i, e := range got {
		if e.FilmSlug == "" {
			t.Errorf("entry %d has empty slug", i)
		}
		// Every row must carry a date: a.month renders only on the first row of
		// each month, so dates have to come from a.daydate's href.
		if e.WatchedOn.IsZero() {
			t.Errorf("entry %d (%s) has no date", i, e.FilmSlug)
		}
		if e.WatchedOn.Year() < 1990 || e.WatchedOn.After(time.Now().AddDate(0, 0, 1)) {
			t.Errorf("entry %d date %v is implausible", i, e.WatchedOn)
		}
		if e.Rating != nil {
			rated++
			if *e.Rating < 0.5 || *e.Rating > 5.0 {
				t.Errorf("entry %d rating %v out of range", i, *e.Rating)
			}
		}
		if e.Rewatch {
			rewatches++
		}
	}
	if rated == 0 {
		t.Error("no ratings parsed from the diary")
	}
	// The fixture's first row is a rewatch; if this is 0 the inverted
	// icon-status-off logic has regressed.
	if rewatches == 0 {
		t.Error("no rewatches detected; check the icon-status-off inversion")
	}
	t.Logf("50 entries: %d rated, %d rewatches; first=%+v", rated, rewatches, got[0])
}

// value="0" on the range input means unrated, not a zero-star rating.
func TestParseDiaryPage_UnratedIsNil(t *testing.T) {
	got, err := ParseDiaryPage(readFixture(t, "diary_page1.html"))
	if err != nil {
		t.Fatalf("ParseDiaryPage: %v", err)
	}
	var unrated int
	for _, e := range got {
		if e.Rating == nil {
			unrated++
			continue
		}
		if *e.Rating == 0 {
			t.Errorf("%s has a zero rating; 0 must map to nil", e.FilmSlug)
		}
	}
	t.Logf("%d of %d diary entries are unrated", unrated, len(got))
}

func TestLastPageNumber(t *testing.T) {
	// The grid fixture's paginator ends at page 36.
	n, err := LastPageNumber(readFixture(t, "films_grid_page1.html"))
	if err != nil {
		t.Fatalf("LastPageNumber: %v", err)
	}
	if n != 36 {
		t.Errorf("LastPageNumber = %d, want 36", n)
	}

	// No paginator at all means a single page.
	n, err = LastPageNumber([]byte("<html><body>no paginator</body></html>"))
	if err != nil {
		t.Fatalf("LastPageNumber: %v", err)
	}
	if n != 1 {
		t.Errorf("LastPageNumber with no paginator = %d, want 1", n)
	}
}

func TestParseFilmStats(t *testing.T) {
	got, err := ParseFilmStats("parasite-2019", readFixture(t, "film_stats_parasite.html"))
	if err != nil {
		t.Fatalf("ParseFilmStats: %v", err)
	}
	// The visible label is abbreviated ("7.6M"); the aria-label holds the exact
	// figure. Anything under a million means we read the lossy one.
	if got.WatchCount < 1_000_000 {
		t.Errorf("WatchCount = %d, want the exact aria-label figure (millions)", got.WatchCount)
	}
	if got.LikeCount < 100_000 {
		t.Errorf("LikeCount = %d, want a large exact figure", got.LikeCount)
	}
	if got.LikeCount > got.WatchCount {
		t.Errorf("LikeCount %d exceeds WatchCount %d", got.LikeCount, got.WatchCount)
	}
	t.Logf("watches=%d likes=%d", got.WatchCount, got.LikeCount)
}

func TestParseFilmStats_NoCountersIsAnError(t *testing.T) {
	if _, err := ParseFilmStats("x", []byte("<div>nothing</div>")); err == nil {
		t.Fatal("want an error when no counters are present")
	}
}

func TestRatingFromClass(t *testing.T) {
	cases := []struct {
		class string
		want  float64
		ok    bool
	}{
		{"rating -micro -darker rated-8", 4.0, true},
		{"rating rated-10", 5.0, true},
		{"rating rated-1", 0.5, true},
		{"rating rated-0", 0, false},
		{"rating", 0, false},
	}
	for _, c := range cases {
		got, ok := ratingFromClass(c.class)
		if ok != c.ok || got != c.want {
			t.Errorf("ratingFromClass(%q) = (%v, %v), want (%v, %v)",
				c.class, got, ok, c.want, c.ok)
		}
	}
}

func TestStripYear(t *testing.T) {
	cases := []struct {
		in    string
		title string
		year  int
	}{
		{"Parasite (2019)", "Parasite", 2019},
		{"Blade Runner 2049 (2017)", "Blade Runner 2049", 2017},
		{"No Year Here", "No Year Here", 0},
	}
	for _, c := range cases {
		gotTitle, gotYear := stripYear(c.in)
		if gotTitle != c.title || gotYear != c.year {
			t.Errorf("stripYear(%q) = (%q, %d), want (%q, %d)",
				c.in, gotTitle, gotYear, c.title, c.year)
		}
	}
}
