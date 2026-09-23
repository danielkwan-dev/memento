package letterboxd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// jsonLD mirrors the schema.org Movie object Letterboxd embeds in every film
// page. Preferring it over CSS selectors makes the parser far more robust:
// structured data changes much less often than markup, and it already carries
// genres, cast, crew, studios, countries and the aggregate rating.
type jsonLD struct {
	Name            string   `json:"name"`
	Duration        string   `json:"duration"`
	DateCreated     string   `json:"dateCreated"`
	Genre           []string `json:"genre"`
	InLanguage      []string `json:"inLanguage"`
	CountryOfOrigin []ldName `json:"countryOfOrigin"`
	ProductionCo    []ldName `json:"productionCompany"`
	Director        []ldName `json:"director"`
	Actor           []ldName `json:"actor"`
	AggregateRating struct {
		RatingValue float64 `json:"ratingValue"`
		RatingCount int64   `json:"ratingCount"`
	} `json:"aggregateRating"`
}

// ldName handles fields that appear either as {"name": "..."} objects or as
// bare strings, which Letterboxd is inconsistent about.
type ldName struct {
	Name string
}

func (n *ldName) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		n.Name = s
		return nil
	}
	var obj struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	n.Name = obj.Name
	return nil
}

var (
	// Letterboxd wraps its JSON-LD in CDATA-ish comment markers.
	ldCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// "PT2H13M" -> 133 minutes.
	durationRe = regexp.MustCompile(`^PT(?:(\d+)H)?(?:(\d+)M)?`)
	// og:title is "Parasite (2019)".
	titleYearRe = regexp.MustCompile(`^(.*?)\s*\((\d{4})\)\s*$`)
	// Fallback runtime from "133 mins" in p.text-link.
	runtimeTextRe = regexp.MustCompile(`([\d,]+)\s*min`)
	trailingYear  = regexp.MustCompile(`\s*\((\d{4})\)\s*$`)
)

// ParseFilmDetails extracts a film's global metadata from its Letterboxd page.
// slug is supplied by the caller because the page does not always state it
// unambiguously.
func ParseFilmDetails(slug string, body []byte) (*FilmDetails, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("parse html for %s: %w", slug, err)
	}

	d := &FilmDetails{Slug: slug}

	// --- Primary source: JSON-LD ---
	if ld := extractJSONLD(doc); ld != nil {
		d.Title = strings.TrimSpace(ld.Name)
		d.RuntimeMin = parseISODuration(ld.Duration)
		d.Genres = dedupeStrings(ld.Genre)
		d.Countries = dedupeStrings(ldNames(ld.CountryOfOrigin))
		d.Studios = dedupeStrings(ldNames(ld.ProductionCo))
		d.AvgRating = ld.AggregateRating.RatingValue
		d.RatingCount = ld.AggregateRating.RatingCount

		if len(ld.DateCreated) >= 4 {
			if y, err := strconv.Atoi(ld.DateCreated[:4]); err == nil {
				d.Year = y
			}
		}
	}

	// --- og:title carries "Title (Year)" and is the most reliable year ---
	if og, ok := doc.Find(`meta[property="og:title"]`).Attr("content"); ok {
		if m := titleYearRe.FindStringSubmatch(og); m != nil {
			if d.Title == "" {
				d.Title = strings.TrimSpace(m[1])
			}
			if y, err := strconv.Atoi(m[2]); err == nil {
				d.Year = y
			}
		} else if d.Title == "" {
			d.Title = strings.TrimSpace(og)
		}
	}

	// --- Runtime fallback: "133 mins" in the footer text ---
	if d.RuntimeMin == 0 {
		txt := doc.Find("p.text-link, p.text-footer").Text()
		if m := runtimeTextRe.FindStringSubmatch(strings.ReplaceAll(txt, " ", " ")); m != nil {
			if v, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", "")); err == nil {
				d.RuntimeMin = v
			}
		}
	}

	// --- Themes are NOT in JSON-LD, so these come from links ---
	d.Themes = dedupeStrings(linkTexts(doc, `a[href*="/films/theme/"], a[href*="/films/mini-theme/"]`))

	// --- Languages: JSON-LD gives ISO codes ("ko"), the page gives display
	// names ("Korean"). Prefer the display names; fall back to codes. ---
	langs := dedupeStrings(linkTexts(doc, `a[href*="/films/language/"]`))
	if len(langs) == 0 {
		langs = dedupeStrings(ldNames(asLdNames(inLanguageStrings(doc))))
	}
	d.Languages = langs

	// --- Genres fallback if JSON-LD was absent ---
	if len(d.Genres) == 0 {
		d.Genres = dedupeStrings(linkTexts(doc, `a[href*="/films/genre/"]`))
	}
	if len(d.Countries) == 0 {
		d.Countries = dedupeStrings(linkTexts(doc, `a[href*="/films/country/"]`))
	}
	if len(d.Studios) == 0 {
		d.Studios = dedupeStrings(linkTexts(doc, `a[href*="/studio/"]`))
	}

	// --- People: read slugs from hrefs so they are stable identifiers ---
	d.Directors = dedupePeople(peopleFrom(doc, `a[href^="/director/"]`, "/director/"))
	cast := dedupePeople(peopleFrom(doc, `#tab-cast a[href^="/actor/"]`, "/actor/"))
	if len(cast) == 0 {
		cast = dedupePeople(peopleFrom(doc, `a[href^="/actor/"]`, "/actor/"))
	}
	// Match the reference implementation: keep only top billing. A film's full
	// cast can exceed 50 people, which adds noise without analytical value.
	if len(cast) > MaxCastMembers {
		cast = cast[:MaxCastMembers]
	}
	for i := range cast {
		cast[i].BillingOrder = i + 1
	}
	d.Cast = cast

	if d.Title == "" {
		return nil, fmt.Errorf("parse %s: no title found (markup may have changed)", slug)
	}
	return d, nil
}

// MaxCastMembers caps how much of the billing order we retain per film.
const MaxCastMembers = 12

func extractJSONLD(doc *goquery.Document) *jsonLD {
	var out *jsonLD
	doc.Find(`script[type="application/ld+json"]`).EachWithBreak(func(_ int, s *goquery.Selection) bool {
		raw := ldCommentRe.ReplaceAllString(s.Text(), "")
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return true
		}
		var ld jsonLD
		if err := json.Unmarshal([]byte(raw), &ld); err != nil {
			return true // try the next block
		}
		if ld.Name != "" || ld.AggregateRating.RatingCount > 0 {
			out = &ld
			return false
		}
		return true
	})
	return out
}

// parseISODuration converts an ISO-8601 duration ("PT2H13M") to whole minutes.
func parseISODuration(s string) int {
	m := durationRe.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	var mins int
	if m[1] != "" {
		h, _ := strconv.Atoi(m[1])
		mins += h * 60
	}
	if m[2] != "" {
		v, _ := strconv.Atoi(m[2])
		mins += v
	}
	return mins
}

func ldNames(in []ldName) []string {
	out := make([]string, 0, len(in))
	for _, n := range in {
		if s := strings.TrimSpace(n.Name); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func asLdNames(in []string) []ldName {
	out := make([]ldName, 0, len(in))
	for _, s := range in {
		out = append(out, ldName{Name: s})
	}
	return out
}

func inLanguageStrings(doc *goquery.Document) []string {
	if ld := extractJSONLD(doc); ld != nil {
		return ld.InLanguage
	}
	return nil
}

func linkTexts(doc *goquery.Document, selector string) []string {
	var out []string
	doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
		if t := strings.TrimSpace(s.Text()); t != "" {
			out = append(out, t)
		}
	})
	return out
}

// peopleFrom reads name + slug pairs, taking the slug from the href so it is a
// stable key even when display names differ across pages.
func peopleFrom(doc *goquery.Document, selector, prefix string) []Person {
	var out []Person
	doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		slug := strings.Trim(strings.TrimPrefix(href, prefix), "/")
		if slug == "" {
			return
		}
		name := strings.TrimSpace(s.Text())
		if name == "" {
			// Cast links use the title attribute when the text is an image.
			name, _ = s.Attr("title")
			name = strings.TrimSpace(name)
		}
		if name == "" {
			return
		}
		out = append(out, Person{Slug: slug, Name: name})
	})
	return out
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}

func dedupePeople(in []Person) []Person {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]Person, 0, len(in))
	for _, p := range in {
		if _, dup := seen[p.Slug]; dup {
			continue
		}
		seen[p.Slug] = struct{}{}
		out = append(out, p)
	}
	return out
}

// stripYear removes a trailing "(2019)" from a display title.
func stripYear(title string) (string, int) {
	if m := trailingYear.FindStringSubmatch(title); m != nil {
		y, _ := strconv.Atoi(m[1])
		return strings.TrimSpace(trailingYear.ReplaceAllString(title, "")), y
	}
	return strings.TrimSpace(title), 0
}
