package letterboxd

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

var (
	// span.rating carries the score as a class: "rated-8" == 4.0 stars.
	ratedClassRe = regexp.MustCompile(`rated-(\d{1,2})`)
	// a.daydate href is "/{user}/diary/films/for/2026/09/25/".
	diaryDateRe = regexp.MustCompile(`/for/(\d{4})/(\d{2})/(\d{2})/`)
	// Pagination links are "/{user}/films/page/36/".
	pageHrefRe = regexp.MustCompile(`/page/(\d+)/?$`)
)

// ParseFilmsGrid reads one page of a user's films grid.
//
// Ratings are taken from the "rated-N" class rather than by counting star
// glyphs: N is on a 0-10 scale, so half-stars are exact integers and there is
// no Unicode counting to get wrong.
func ParseFilmsGrid(body []byte) ([]WatchEntry, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse films grid: %w", err)
	}

	items := doc.Find("li.griditem")
	if items.Length() == 0 {
		// An empty page is legitimate (past the last page); the caller decides.
		return nil, nil
	}

	out := make([]WatchEntry, 0, items.Length())
	items.Each(func(_ int, li *goquery.Selection) {
		comp := li.Find("div.react-component").First()
		slug, _ := comp.Attr("data-item-slug")
		if slug == "" {
			return
		}
		name, _ := comp.Attr("data-item-name")
		title, year := stripYear(name)

		e := WatchEntry{FilmSlug: slug, Title: title, Year: year}

		// Viewing data lives in a sibling <p>, not inside the react component.
		vd := li.Find("p.poster-viewingdata")
		if cls, ok := vd.Find("span.rating").Attr("class"); ok {
			if r, ok := ratingFromClass(cls); ok {
				e.Rating = &r
			}
		}
		e.Liked = vd.Find("span.like").Length() > 0

		out = append(out, e)
	})
	return out, nil
}

// ParseDiaryPage reads one page of a user's diary.
func ParseDiaryPage(body []byte) ([]DiaryEntry, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse diary: %w", err)
	}

	rows := doc.Find("tr.diary-entry-row")
	if rows.Length() == 0 {
		return nil, nil
	}

	out := make([]DiaryEntry, 0, rows.Length())
	rows.Each(func(_ int, tr *goquery.Selection) {
		comp := tr.Find("div.react-component").First()
		slug, _ := comp.Attr("data-item-slug")
		if slug == "" {
			return
		}
		name, _ := comp.Attr("data-item-name")
		title, yearFromName := stripYear(name)

		e := DiaryEntry{FilmSlug: slug, Title: title, Year: yearFromName}

		// The release year has its own column; prefer it over the display name.
		if y := strings.TrimSpace(tr.Find("td.col-releaseyear span").First().Text()); y != "" {
			if v, err := strconv.Atoi(y); err == nil {
				e.Year = v
			}
		}

		// Date: a.month only renders on the first row of each month, so the
		// full date must come from a.daydate's href, which is always present.
		if href, ok := tr.Find("a.daydate").Attr("href"); ok {
			if d, ok := dateFromDiaryHref(href); ok {
				e.WatchedOn = d
			}
		}

		// Rating is a 0-10 range input; 0 means "not rated", not zero stars.
		if v, ok := tr.Find("input.rateit-field").First().Attr("value"); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				r := float64(n) / 2
				e.Rating = &r
			}
		}

		// Rewatch is marked by the ABSENCE of "icon-status-off" on the cell:
		// the icon is always rendered and merely dimmed when it does not apply.
		if td := tr.Find("td.col-rewatch"); td.Length() > 0 {
			cls, _ := td.Attr("class")
			e.Rewatch = !strings.Contains(cls, "icon-status-off")
		}

		if e.WatchedOn.IsZero() {
			// Without a date the row is useless for every time-series chart.
			return
		}
		out = append(out, e)
	})
	return out, nil
}

// LastPageNumber reports the highest page number linked in the paginator, so
// the caller can fan out over pages concurrently instead of walking serially.
// Returns 1 when the page has no paginator.
func LastPageNumber(body []byte) (int, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return 0, fmt.Errorf("parse paginator: %w", err)
	}
	last := 1
	doc.Find("div.paginate-pages a").Each(func(_ int, a *goquery.Selection) {
		href, _ := a.Attr("href")
		if m := pageHrefRe.FindStringSubmatch(href); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > last {
				last = n
			}
		}
	})
	return last, nil
}

// ratingFromClass turns "rating -micro -darker rated-8" into 4.0.
func ratingFromClass(class string) (float64, bool) {
	m := ratedClassRe.FindStringSubmatch(class)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 || n > 10 {
		return 0, false
	}
	return float64(n) / 2, true
}

func dateFromDiaryHref(href string) (time.Time, bool) {
	m := diaryDateRe.FindStringSubmatch(href)
	if m == nil {
		return time.Time{}, false
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d, _ := strconv.Atoi(m[3])
	if y == 0 || mo == 0 || d == 0 {
		return time.Time{}, false
	}
	return time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC), true
}
