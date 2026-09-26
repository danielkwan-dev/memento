package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielkwan-dev/memento/internal/config"
	"github.com/danielkwan-dev/memento/internal/stats"
)

// newTestServer builds a Server with no database. Only the handlers that never
// touch the store are exercised here; anything database-backed is covered by the
// store and stats tests against real Postgres.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		cfg: &config.Config{
			CORSOrigins: []string{"http://localhost:5173"},
		},
		log:   quietLogger(),
		waker: NewWaker("", quietLogger()),
	}
}

func TestHealthz_RespondsWithoutDatabase(t *testing.T) {
	// /healthz must not depend on the database: it is what the frontend calls on
	// page load to wake a scaled-to-zero machine, and it has to answer during
	// boot, before the pool is necessarily usable.
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %v, want ok", body["status"])
	}
	if _, ok := body["time"]; !ok {
		t.Error("response has no time field")
	}
}

func TestSync_RejectsBadInput(t *testing.T) {
	srv := newTestServer(t)

	cases := []struct {
		name string
		body string
		want int
	}{
		{"not json", `not json at all`, http.StatusBadRequest},
		{"empty username", `{"username":""}`, http.StatusBadRequest},
		{"too short", `{"username":"a"}`, http.StatusBadRequest},
		{"path traversal", `{"username":"../admin"}`, http.StatusBadRequest},
		{"url", `{"username":"https://letterboxd.com/dave/"}`, http.StatusBadRequest},
		{"missing field", `{}`, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/sync", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			// Validation runs before any store access, so a nil store is fine for
			// these cases: reaching the store would itself be the bug.
			srv.handleSync(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
			var body map[string]string
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["error"] == "" {
				t.Error("error response has no message")
			}
		})
	}
}

func TestImport_RejectsBadUploads(t *testing.T) {
	srv := newTestServer(t)

	t.Run("not multipart", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/import", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleImport(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})
}

// Every declared stats category must be routable, so the frontend's per-category
// lazy loading cannot request a path the server rejects.
func TestStatsCategoryRouting(t *testing.T) {
	for _, cat := range stats.AllCategories {
		name := string(cat)
		if name == "" {
			t.Error("empty category name")
		}
		// Category names appear in URLs, so they must be path-safe.
		if strings.ContainsAny(name, "/?#% ") {
			t.Errorf("category %q is not URL-safe", name)
		}
		// Round-tripping is what the handler relies on to reject unknown paths.
		if parsed, ok := stats.ParseCategory(name); !ok || parsed != cat {
			t.Errorf("ParseCategory(%q) did not round-trip", name)
		}
	}
	if _, ok := stats.ParseCategory("nope"); ok {
		t.Error("ParseCategory accepted an unknown category")
	}
}
