package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// testDist mimics a Vite build: a hashed asset under assets/ plus index.html.
func testDist() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{
			Data: []byte(`<!doctype html><html><head>` +
				`<script src="/assets/index-abc123.js"></script></head><body></body></html>`),
		},
		"assets/index-abc123.js":  &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/index-abc123.css": &fstest.MapFile{Data: []byte("body{}")},
		"favicon.svg":             &fstest.MapFile{Data: []byte("<svg/>")},
	}
}

// http.FileServer 301-redirects "/index.html" to "./", which loops forever once
// the handler has rewritten the path. That bug shipped in the container and
// showed up as an empty response at "/", so it gets a test.
func TestStatic_ServesIndexWithoutRedirecting(t *testing.T) {
	h := staticHandler(testDist())

	for _, path := range []string{"/", "/index.html"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (a 3xx here means a redirect loop)", path, rec.Code)
		}
		if body := rec.Body.String(); len(body) == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
		if ct := rec.Header().Get("Content-Type"); ct == "" || ct[:9] != "text/html" {
			t.Errorf("GET %s content-type = %q, want text/html", path, ct)
		}
	}
}

// Client-side routing means the server has no list of valid routes, so unknown
// paths must hand back the SPA entry point.
func TestStatic_UnknownRouteFallsBackToIndex(t *testing.T) {
	h := staticHandler(testDist())

	for _, path := range []string{"/dashboard", "/users/dave", "/deep/nested/route"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 via SPA fallback", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct[:9] != "text/html" {
			t.Errorf("GET %s served %q, want html", path, ct)
		}
	}
}

// A missing asset must 404 rather than fall back to HTML: serving markup for a
// .js request produces a confusing MIME error instead of an honest failure.
func TestStatic_MissingAssetIs404(t *testing.T) {
	h := staticHandler(testDist())

	for _, path := range []string{"/assets/gone.js", "/assets/gone.css", "/missing.svg"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (not an HTML fallback)", path, rec.Code)
		}
	}
}

// Hashed filenames are immutable, but index.html carries the asset references, so
// a cached copy would point at files that no longer exist after a deploy.
func TestStatic_CacheHeaders(t *testing.T) {
	h := staticHandler(testDist())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil))
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("asset Cache-Control = %q, want long-lived immutable", cc)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("index Cache-Control = %q, want no-cache", cc)
	}
}

func TestStatic_ServesRealAssets(t *testing.T) {
	h := staticHandler(testDist())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("asset = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "console.log(1)" {
		t.Errorf("asset body = %q, want the file contents", got)
	}
}

// Path traversal must not escape the embedded filesystem.
func TestStatic_RejectsTraversal(t *testing.T) {
	h := staticHandler(testDist())

	for _, path := range []string{"/../secret", "/assets/../../etc/passwd"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		// path.Clean collapses these, so they land on the SPA fallback or a 404;
		// either is fine, leaking a file outside the FS is not.
		if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 200 or 404", path, rec.Code)
		}
		if body := rec.Body.String(); len(body) > 0 && body[0] == 'r' {
			t.Errorf("GET %s may have leaked a file: %.40q", path, body)
		}
	}
}

// With no frontend embedded (ordinary local builds) the API must still route.
func TestStatic_NotRegisteredWhenAbsent(t *testing.T) {
	if StaticFS != nil {
		t.Skip("a frontend is embedded in this build")
	}
	// Routes() only mounts the catch-all when StaticFS is set, so an unknown path
	// must 404 rather than panic.
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/not-a-route", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path without a frontend = %d, want 404", rec.Code)
	}
}
