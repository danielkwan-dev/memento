package letterboxd

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Export is a parsed Letterboxd data export (Settings -> Data -> Export Your
// Data), which arrives as a ZIP of CSVs.
//
// This path exists because it avoids the paginated user pages entirely -- the
// uncached, most-blocked requests in the whole scrape. Film metadata still has to
// be fetched, but only for films the global cache is missing.
type Export struct {
	Watched []WatchEntry
	Diary   []DiaryEntry
}

// filmURIRe pulls a slug out of the URI column, which is either a full film URL
// or a boxd.it short link.
var filmURIRe = regexp.MustCompile(`letterboxd\.com/film/([^/?#]+)`)

// ParseExportZip reads watched.csv, ratings.csv, diary.csv and likes/films.csv
// out of an export archive.
//
// Column headers have changed casing and spacing across Letterboxd versions, so
// lookups are normalised rather than exact.
func ParseExportZip(data []byte) (*Export, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open export zip: %w", err)
	}

	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		// Normalise separators and case so nested paths match regardless of how
		// the archive was produced.
		key := strings.ToLower(strings.ReplaceAll(f.Name, "\\", "/"))
		files[key] = f
	}

	find := func(suffixes ...string) *zip.File {
		for _, want := range suffixes {
			for name, f := range files {
				if name == want || strings.HasSuffix(name, "/"+want) {
					return f
				}
			}
		}
		return nil
	}

	exp := &Export{}

	// watched.csv is the authoritative list of logged films.
	watched := find("watched.csv")
	if watched == nil {
		return nil, fmt.Errorf("export zip has no watched.csv (is this a Letterboxd export?)")
	}
	byslug := map[string]*WatchEntry{}
	if err := eachRow(watched, func(get func(string) string) {
		slug := slugFromURI(get("letterboxduri"))
		if slug == "" {
			return
		}
		year, _ := strconv.Atoi(get("year"))
		e := &WatchEntry{
			FilmSlug: slug,
			Title:    strings.TrimSpace(get("name")),
			Year:     year,
		}
		byslug[slug] = e
	}); err != nil {
		return nil, fmt.Errorf("parse watched.csv: %w", err)
	}

	// ratings.csv carries the star ratings for the subset that has them.
	if f := find("ratings.csv"); f != nil {
		if err := eachRow(f, func(get func(string) string) {
			slug := slugFromURI(get("letterboxduri"))
			e, ok := byslug[slug]
			if !ok {
				return
			}
			if r, err := strconv.ParseFloat(strings.TrimSpace(get("rating")), 64); err == nil && r > 0 {
				e.Rating = &r
			}
		}); err != nil {
			return nil, fmt.Errorf("parse ratings.csv: %w", err)
		}
	}

	// likes/films.csv marks liked films.
	if f := find("films.csv", "likes/films.csv"); f != nil && strings.Contains(strings.ToLower(f.Name), "like") {
		if err := eachRow(f, func(get func(string) string) {
			if e, ok := byslug[slugFromURI(get("letterboxduri"))]; ok {
				e.Liked = true
			}
		}); err != nil {
			return nil, fmt.Errorf("parse likes/films.csv: %w", err)
		}
	}

	for _, e := range byslug {
		exp.Watched = append(exp.Watched, *e)
	}

	// diary.csv supplies the dated entries, including rewatches.
	if f := find("diary.csv"); f != nil {
		if err := eachRow(f, func(get func(string) string) {
			slug := slugFromURI(get("letterboxduri"))
			if slug == "" {
				return
			}
			watchedOn, err := time.Parse("2006-01-02", strings.TrimSpace(get("watcheddate")))
			if err != nil {
				// Without a date the row is useless for every time-series chart.
				return
			}
			year, _ := strconv.Atoi(get("year"))
			d := DiaryEntry{
				FilmSlug:  slug,
				Title:     strings.TrimSpace(get("name")),
				Year:      year,
				WatchedOn: watchedOn,
				Rewatch:   isTruthy(get("rewatch")),
			}
			if r, err := strconv.ParseFloat(strings.TrimSpace(get("rating")), 64); err == nil && r > 0 {
				d.Rating = &r
			}
			exp.Diary = append(exp.Diary, d)
		}); err != nil {
			return nil, fmt.Errorf("parse diary.csv: %w", err)
		}
	}

	if len(exp.Watched) == 0 && len(exp.Diary) == 0 {
		return nil, fmt.Errorf("export contained no films")
	}
	return exp, nil
}

// eachRow streams a CSV member, calling fn with a normalised column accessor.
func eachRow(f *zip.File, fn func(get func(string) string)) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	r := csv.NewReader(rc)
	// Letterboxd exports have had inconsistent column counts across versions.
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	index := make(map[string]int, len(header))
	for i, h := range header {
		index[normaliseHeader(h)] = i
	}

	for {
		row, err := r.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// A single malformed line should not abandon the whole import.
			continue
		}
		fn(func(name string) string {
			i, ok := index[name]
			if !ok || i >= len(row) {
				return ""
			}
			return row[i]
		})
	}
}

// normaliseHeader strips everything but letters and lowercases, so "Letterboxd
// URI", "letterboxd_uri" and "LetterboxdURI" all match.
func normaliseHeader(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// slugFromURI extracts a film slug from a Letterboxd URL. Short boxd.it links
// cannot be resolved without a network round trip, so they are reported as empty
// and handled by the caller.
func slugFromURI(uri string) string {
	if m := filmURIRe.FindStringSubmatch(uri); m != nil {
		return m[1]
	}
	return ""
}

// ShortLinkIDs returns the boxd.it short links in an export that still need
// resolving to film slugs.
func ShortLinkIDs(uris []string) []string {
	var out []string
	for _, u := range uris {
		if strings.Contains(u, "boxd.it/") && slugFromURI(u) == "" {
			out = append(out, u)
		}
	}
	return out
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "true", "1", "y":
		return true
	}
	return false
}
