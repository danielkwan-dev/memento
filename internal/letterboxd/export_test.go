package letterboxd

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

// buildExportZip synthesizes an export archive. Generating it beats committing a
// real one: a real export is personal data, and the shapes worth testing are the
// header variations and edge cases, which are easy to construct.
func buildExportZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestParseExportZip(t *testing.T) {
	data := buildExportZip(t, map[string]string{
		"watched.csv": `Date,Name,Year,Letterboxd URI
2024-01-02,Parasite,2019,https://letterboxd.com/film/parasite-2019/
2024-01-03,Whiplash,2014,https://letterboxd.com/film/whiplash-2014/
2024-01-04,The General,1926,https://letterboxd.com/film/the-general/
`,
		"ratings.csv": `Date,Name,Year,Letterboxd URI,Rating
2024-01-02,Parasite,2019,https://letterboxd.com/film/parasite-2019/,4.5
2024-01-03,Whiplash,2014,https://letterboxd.com/film/whiplash-2014/,5
`,
		"likes/films.csv": `Date,Name,Year,Letterboxd URI
2024-01-02,Parasite,2019,https://letterboxd.com/film/parasite-2019/
`,
		"diary.csv": `Date,Name,Year,Letterboxd URI,Rating,Rewatch,Tags,Watched Date
2024-03-01,Parasite,2019,https://letterboxd.com/film/parasite-2019/,4.5,No,,2024-03-01
2024-05-09,Parasite,2019,https://letterboxd.com/film/parasite-2019/,4.5,Yes,,2024-05-09
2024-06-01,Whiplash,2014,https://letterboxd.com/film/whiplash-2014/,5,No,,2024-06-01
`,
	})

	exp, err := ParseExportZip(data)
	if err != nil {
		t.Fatalf("ParseExportZip: %v", err)
	}

	if len(exp.Watched) != 3 {
		t.Errorf("Watched = %d entries, want 3", len(exp.Watched))
	}

	bySlug := map[string]WatchEntry{}
	for _, e := range exp.Watched {
		bySlug[e.FilmSlug] = e
	}

	parasite, ok := bySlug["parasite-2019"]
	if !ok {
		t.Fatalf("parasite-2019 missing; got %v", bySlug)
	}
	if parasite.Title != "Parasite" || parasite.Year != 2019 {
		t.Errorf("parasite = %+v, want Parasite/2019", parasite)
	}
	if parasite.Rating == nil || *parasite.Rating != 4.5 {
		t.Errorf("parasite rating = %v, want 4.5", parasite.Rating)
	}
	if !parasite.Liked {
		t.Error("parasite should be liked")
	}

	// The General is in watched.csv but not ratings.csv, so it must stay unrated
	// rather than defaulting to zero.
	general := bySlug["the-general"]
	if general.Rating != nil {
		t.Errorf("the-general rating = %v, want nil (absent from ratings.csv)", general.Rating)
	}
	if general.Liked {
		t.Error("the-general should not be liked")
	}

	// Diary: three rows, one of them a rewatch of the same film on a later date.
	if len(exp.Diary) != 3 {
		t.Fatalf("Diary = %d entries, want 3", len(exp.Diary))
	}
	var rewatches, parasiteRows int
	for _, d := range exp.Diary {
		if d.Rewatch {
			rewatches++
		}
		if d.FilmSlug == "parasite-2019" {
			parasiteRows++
		}
		if d.WatchedOn.IsZero() {
			t.Errorf("diary entry %s has no date", d.FilmSlug)
		}
	}
	if rewatches != 1 {
		t.Errorf("rewatches = %d, want 1", rewatches)
	}
	if parasiteRows != 2 {
		t.Errorf("parasite diary rows = %d, want 2 (a film can be logged twice)", parasiteRows)
	}
}

// Headers have changed casing and spacing across Letterboxd versions, so lookups
// are normalised. This pins that down.
func TestParseExportZip_ToleratesHeaderVariations(t *testing.T) {
	variants := []string{
		"Date,Name,Year,Letterboxd URI",
		"date,name,year,letterboxd_uri",
		"Date,Name,Year,LetterboxdURI",
		"DATE,NAME,YEAR,LETTERBOXD URI",
	}

	for _, header := range variants {
		t.Run(header, func(t *testing.T) {
			data := buildExportZip(t, map[string]string{
				"watched.csv": header + "\n2024-01-02,Parasite,2019,https://letterboxd.com/film/parasite-2019/\n",
			})
			exp, err := ParseExportZip(data)
			if err != nil {
				t.Fatalf("ParseExportZip: %v", err)
			}
			if len(exp.Watched) != 1 {
				t.Fatalf("Watched = %d, want 1", len(exp.Watched))
			}
			if exp.Watched[0].FilmSlug != "parasite-2019" {
				t.Errorf("slug = %q, want parasite-2019", exp.Watched[0].FilmSlug)
			}
			if exp.Watched[0].Title != "Parasite" {
				t.Errorf("title = %q, want Parasite", exp.Watched[0].Title)
			}
		})
	}
}

func TestParseExportZip_SkipsUndatedDiaryRows(t *testing.T) {
	data := buildExportZip(t, map[string]string{
		"watched.csv": `Date,Name,Year,Letterboxd URI
2024-01-02,Parasite,2019,https://letterboxd.com/film/parasite-2019/
`,
		"diary.csv": `Date,Name,Year,Letterboxd URI,Rating,Rewatch,Watched Date
2024-03-01,Parasite,2019,https://letterboxd.com/film/parasite-2019/,4.5,No,
2024-03-02,Parasite,2019,https://letterboxd.com/film/parasite-2019/,4.5,No,2024-03-02
`,
	})

	exp, err := ParseExportZip(data)
	if err != nil {
		t.Fatalf("ParseExportZip: %v", err)
	}
	// Only the dated row survives.
	if len(exp.Diary) != 1 {
		t.Errorf("Diary = %d entries, want 1 (undated rows dropped)", len(exp.Diary))
	}
}

// A rating of 0 in the CSV means unrated, the same as the diary's range input.
func TestParseExportZip_ZeroRatingIsUnrated(t *testing.T) {
	data := buildExportZip(t, map[string]string{
		"watched.csv": `Name,Year,Letterboxd URI
Parasite,2019,https://letterboxd.com/film/parasite-2019/
`,
		"ratings.csv": `Name,Year,Letterboxd URI,Rating
Parasite,2019,https://letterboxd.com/film/parasite-2019/,0
`,
	})

	exp, err := ParseExportZip(data)
	if err != nil {
		t.Fatalf("ParseExportZip: %v", err)
	}
	if exp.Watched[0].Rating != nil {
		t.Errorf("rating = %v, want nil for a 0 value", exp.Watched[0].Rating)
	}
}

func TestParseExportZip_RejectsNonExport(t *testing.T) {
	data := buildExportZip(t, map[string]string{
		"something-else.csv": "a,b,c\n1,2,3\n",
	})
	_, err := ParseExportZip(data)
	if err == nil {
		t.Fatal("want an error when watched.csv is absent")
	}
	if !strings.Contains(err.Error(), "watched.csv") {
		t.Errorf("error should mention watched.csv, got: %v", err)
	}
}

func TestParseExportZip_RejectsGarbage(t *testing.T) {
	if _, err := ParseExportZip([]byte("not a zip file at all")); err == nil {
		t.Fatal("want an error for a non-zip payload")
	}
}

// boxd.it short links cannot be resolved without a network round trip, so they
// are surfaced rather than silently dropped.
func TestSlugFromURI(t *testing.T) {
	cases := map[string]string{
		"https://letterboxd.com/film/parasite-2019/":     "parasite-2019",
		"http://letterboxd.com/film/whiplash-2014":       "whiplash-2014",
		"https://letterboxd.com/film/the-general/?foo=1": "the-general",
		"https://boxd.it/1Ax4":                           "",
		"":                                               "",
	}
	for in, want := range cases {
		if got := slugFromURI(in); got != want {
			t.Errorf("slugFromURI(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortLinkIDs(t *testing.T) {
	got := ShortLinkIDs([]string{
		"https://letterboxd.com/film/parasite-2019/",
		"https://boxd.it/1Ax4",
		"https://boxd.it/2By5",
	})
	if len(got) != 2 {
		t.Errorf("ShortLinkIDs = %v, want the 2 boxd.it links", got)
	}
}

func TestNormaliseHeader(t *testing.T) {
	cases := map[string]string{
		"Letterboxd URI": "letterboxduri",
		"letterboxd_uri": "letterboxduri",
		"LetterboxdURI":  "letterboxduri",
		"Watched Date":   "watcheddate",
		"Rating":         "rating",
	}
	for in, want := range cases {
		if got := normaliseHeader(in); got != want {
			t.Errorf("normaliseHeader(%q) = %q, want %q", in, got, want)
		}
	}
}
