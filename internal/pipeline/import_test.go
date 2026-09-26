package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// buildExport makes a minimal Letterboxd export archive with n films.
func buildExport(t *testing.T, n int) []byte {
	t.Helper()
	var watched bytes.Buffer
	watched.WriteString("Date,Name,Year,Letterboxd URI\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&watched, "2024-01-01,Film %d,2020,https://letterboxd.com/film/film-%d/\n", i, i)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("watched.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(watched.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// An import must survive upstream throttling.
//
// The film list came from the user's own export, so it is already complete --
// fetching details only adds genres, runtimes and cast. Failing the whole import
// over missing metadata discards good data, and (observed in production) told the
// user to "upload an export instead" when that is exactly what they had done.
func TestRunImport_SurvivesHydrationFailure(t *testing.T) {
	f := newFakeFetcher()
	// Every film detail fetch fails: the worst case of upstream throttling.
	for i := 0; i < 60; i++ {
		f.failSlugs[fmt.Sprintf("film-%d", i)] = true
	}
	s := newFakeStore()

	res, err := quietPipeline(f, s, testConfig()).
		RunImport(context.Background(), uuid.New(), "someone", buildExport(t, 60))
	if err != nil {
		t.Fatalf("an import must not fail when only hydration fails: %v", err)
	}

	// The export's film list is what matters, and it must be persisted in full.
	if res.WatchEntries != 60 {
		t.Errorf("WatchEntries = %d, want 60", res.WatchEntries)
	}
	if len(s.watchEntries) != 60 {
		t.Errorf("persisted %d watch entries, want all 60", len(s.watchEntries))
	}
	if s.syncedUser == 0 {
		t.Error("user was not marked synced despite a usable import")
	}
	// The failures are still reported, so the UI can explain what is missing.
	if res.FilmFailures == 0 {
		t.Error("FilmFailures = 0, want the failures to be reported")
	}
}

func TestRunImport_HappyPath(t *testing.T) {
	f := newFakeFetcher()
	s := newFakeStore()

	res, err := quietPipeline(f, s, testConfig()).
		RunImport(context.Background(), uuid.New(), "someone", buildExport(t, 12))
	if err != nil {
		t.Fatalf("RunImport: %v", err)
	}
	if res.WatchEntries != 12 {
		t.Errorf("WatchEntries = %d, want 12", res.WatchEntries)
	}
	if res.FilmsFetched != 12 {
		t.Errorf("FilmsFetched = %d, want 12", res.FilmsFetched)
	}
	if res.FilmFailures != 0 {
		t.Errorf("FilmFailures = %d, want 0", res.FilmFailures)
	}
}

func TestRunImport_RejectsGarbage(t *testing.T) {
	f := newFakeFetcher()
	s := newFakeStore()

	if _, err := quietPipeline(f, s, testConfig()).
		RunImport(context.Background(), uuid.New(), "someone", []byte("not a zip")); err == nil {
		t.Fatal("want an error for a non-zip payload")
	}
}
