package letterboxd

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// numberInLabel pulls the first grouped number out of an aria-label such as
// "Watched by 7,596,439 members".
var numberInLabel = regexp.MustCompile(`([\d][\d,\.]*)`)

// ParseFilmStats reads the popularity counters from the /csi/film/{slug}/stats/
// fragment.
//
// The visible <span class="label"> is abbreviated and lossy ("7.6M"), so the
// exact figure is taken from the aria-label instead. Precision matters here:
// these counts drive the obscurity percentile.
func ParseFilmStats(slug string, body []byte) (*FilmStats, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse stats for %s: %w", slug, err)
	}

	s := &FilmStats{Slug: slug}
	var found bool

	doc.Find("div.production-statistic").Each(func(_ int, sel *goquery.Selection) {
		class, _ := sel.Attr("class")
		label, _ := sel.Attr("aria-label")
		if label == "" {
			// Fall back to the anchor's title attribute.
			label, _ = sel.Find("a").Attr("title")
		}
		n, ok := parseGroupedNumber(label)
		if !ok {
			return
		}
		switch {
		case strings.Contains(class, "-watches"):
			s.WatchCount = n
			found = true
		case strings.Contains(class, "-likes"):
			s.LikeCount = n
			found = true
		}
	})

	if !found {
		return nil, fmt.Errorf("parse stats for %s: no watch/like counters found (markup may have changed)", slug)
	}
	return s, nil
}

// parseGroupedNumber turns "Watched by 7,596,439 members" into 7596439.
func parseGroupedNumber(label string) (int64, bool) {
	if label == "" {
		return 0, false
	}
	// Letterboxd uses non-breaking spaces between the number and the noun.
	label = strings.ReplaceAll(label, " ", " ")
	m := numberInLabel.FindStringSubmatch(label)
	if m == nil {
		return 0, false
	}
	clean := strings.NewReplacer(",", "", ".", "").Replace(m[1])
	n, err := strconv.ParseInt(clean, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
