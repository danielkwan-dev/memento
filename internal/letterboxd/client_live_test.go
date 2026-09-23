package letterboxd

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TestLiveFetch hits the real Letterboxd. It is skipped unless MEMENTO_LIVE=1 so
// the default test suite stays offline, fast and deterministic.
//
//	MEMENTO_LIVE=1 go test ./internal/letterboxd/ -run TestLiveFetch -v
func TestLiveFetch(t *testing.T) {
	if os.Getenv("MEMENTO_LIVE") != "1" {
		t.Skip("set MEMENTO_LIVE=1 to run live network tests")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, err := NewClient(DefaultConfig(), log)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	cases := []struct {
		name string
		url  string
	}{
		// Plain curl gets 200 here.
		{"film page", BaseURL + "/film/parasite-2019/"},
		// Plain curl gets 403 here: this is the case TLS spoofing must fix.
		{"stats endpoint", BaseURL + "/csi/film/parasite-2019/stats/"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			body, err := c.Get(ctx, tc.url)
			if err != nil {
				t.Fatalf("Get(%s): %v", tc.url, err)
			}
			if len(body) == 0 {
				t.Fatalf("Get(%s): empty body", tc.url)
			}
			t.Logf("OK %s -> %d bytes", tc.url, len(body))
		})
	}

	req, retries, blocks, switches := c.Snapshot()
	t.Logf("stats: requests=%d retries=%d blocks=%d profile_switches=%d",
		req, retries, blocks, switches)
}
