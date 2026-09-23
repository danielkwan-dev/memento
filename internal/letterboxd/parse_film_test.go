package letterboxd

import (
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v (run: go run ./cmd/fixtures -all)", name, err)
	}
	return b
}

func TestParseFilmDetails_Parasite(t *testing.T) {
	got, err := ParseFilmDetails("parasite-2019", readFixture(t, "film_parasite.html"))
	if err != nil {
		t.Fatalf("ParseFilmDetails: %v", err)
	}

	if got.Title != "Parasite" {
		t.Errorf("Title = %q, want %q", got.Title, "Parasite")
	}
	if got.Year != 2019 {
		t.Errorf("Year = %d, want 2019", got.Year)
	}
	if got.RuntimeMin != 133 {
		t.Errorf("RuntimeMin = %d, want 133", got.RuntimeMin)
	}

	wantGenres := map[string]bool{"Thriller": true, "Comedy": true, "Drama": true}
	if len(got.Genres) != 3 {
		t.Errorf("Genres = %v, want 3 entries", got.Genres)
	}
	for _, g := range got.Genres {
		if !wantGenres[g] {
			t.Errorf("unexpected genre %q in %v", g, got.Genres)
		}
	}

	if len(got.Directors) != 1 {
		t.Fatalf("Directors = %v, want exactly 1 (deduped)", got.Directors)
	}
	if got.Directors[0].Slug != "bong-joon-ho" {
		t.Errorf("director slug = %q, want bong-joon-ho", got.Directors[0].Slug)
	}

	// Cast must be capped and billed in order.
	if len(got.Cast) != MaxCastMembers {
		t.Errorf("len(Cast) = %d, want %d", len(got.Cast), MaxCastMembers)
	}
	if len(got.Cast) > 0 {
		if got.Cast[0].Slug != "song-kang-ho" {
			t.Errorf("top-billed = %q, want song-kang-ho", got.Cast[0].Slug)
		}
		if got.Cast[0].BillingOrder != 1 {
			t.Errorf("BillingOrder = %d, want 1", got.Cast[0].BillingOrder)
		}
	}

	if len(got.Countries) == 0 || got.Countries[0] != "South Korea" {
		t.Errorf("Countries = %v, want [South Korea]", got.Countries)
	}
	if len(got.Studios) == 0 {
		t.Error("Studios is empty, want at least one")
	}
	if len(got.Themes) == 0 {
		t.Error("Themes is empty; themes come from links, not JSON-LD")
	}

	// Languages must be display names, deduped (the page lists Korean twice).
	if len(got.Languages) == 0 {
		t.Fatal("Languages is empty")
	}
	for _, l := range got.Languages {
		if len(l) <= 3 {
			t.Errorf("language %q looks like an ISO code, want a display name", l)
		}
	}
	seen := map[string]int{}
	for _, l := range got.Languages {
		seen[l]++
		if seen[l] > 1 {
			t.Errorf("duplicate language %q in %v", l, got.Languages)
		}
	}

	// Volatile: assert sane ranges, never exact values.
	if got.AvgRating < 1 || got.AvgRating > 5 {
		t.Errorf("AvgRating = %v, want within 1..5", got.AvgRating)
	}
	if got.RatingCount <= 0 {
		t.Errorf("RatingCount = %d, want > 0", got.RatingCount)
	}

	t.Logf("parsed: %s (%d) %dmin genres=%v langs=%v themes=%d cast=%d avg=%.2f",
		got.Title, got.Year, got.RuntimeMin, got.Genres, got.Languages,
		len(got.Themes), len(got.Cast), got.AvgRating)
}

func TestParseFilmDetails_MultiDirector(t *testing.T) {
	got, err := ParseFilmDetails("everything-everywhere-all-at-once",
		readFixture(t, "film_multi_director.html"))
	if err != nil {
		t.Fatalf("ParseFilmDetails: %v", err)
	}
	if len(got.Directors) != 2 {
		t.Errorf("Directors = %+v, want 2", got.Directors)
	}
	if got.Title == "" || got.Year == 0 {
		t.Errorf("Title/Year not parsed: %q %d", got.Title, got.Year)
	}
	t.Logf("parsed: %s (%d) directors=%+v", got.Title, got.Year, got.Directors)
}

// A silent film exercises the "no spoken language" edge case, which is where a
// naive parser panics or invents data.
func TestParseFilmDetails_Silent(t *testing.T) {
	got, err := ParseFilmDetails("the-general", readFixture(t, "film_silent.html"))
	if err != nil {
		t.Fatalf("ParseFilmDetails: %v", err)
	}
	if got.Title == "" {
		t.Error("Title is empty")
	}
	if got.RuntimeMin <= 0 {
		t.Errorf("RuntimeMin = %d, want > 0", got.RuntimeMin)
	}
	t.Logf("parsed: %s (%d) %dmin langs=%v countries=%v",
		got.Title, got.Year, got.RuntimeMin, got.Languages, got.Countries)
}

func TestParseFilmDetails_NoTitleIsAnError(t *testing.T) {
	_, err := ParseFilmDetails("nope", []byte("<html><body>nothing here</body></html>"))
	if err == nil {
		t.Fatal("want an error when the page has no title, got nil")
	}
}

func TestParseISODuration(t *testing.T) {
	cases := map[string]int{
		"PT2H13M": 133,
		"PT1H":    60,
		"PT45M":   45,
		"":        0,
		"garbage": 0,
	}
	for in, want := range cases {
		if got := parseISODuration(in); got != want {
			t.Errorf("parseISODuration(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestLooksLikeChallenge(t *testing.T) {
	// A real Cloudflare interstitial captured from a blocked request.
	if !looksLikeChallenge(readFixture(t, "cloudflare_challenge.html")) {
		t.Error("real challenge page was not detected")
	}
	// A real film page must never be mistaken for a challenge.
	if looksLikeChallenge(readFixture(t, "film_parasite.html")) {
		t.Error("film page misdetected as a challenge")
	}
}
