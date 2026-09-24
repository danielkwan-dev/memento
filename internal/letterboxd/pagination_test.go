package letterboxd

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TestLivePagination probes how paginated user pages behave compared with page 1.
// Plain curl gets 403 on every /page/N/ URL while page 1 returns 200, so this
// checks whether the spoofed client clears them and how much pacing it needs.
//
//	MEMENTO_LIVE=1 go test ./internal/letterboxd/ -run TestLivePagination -v
func TestLivePagination(t *testing.T) {
	if os.Getenv("MEMENTO_LIVE") != "1" {
		t.Skip("set MEMENTO_LIVE=1 to run live network tests")
	}

	user := os.Getenv("MEMENTO_LIVE_USER")
	if user == "" {
		user = "dave"
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c, err := NewClient(DefaultConfig(), log)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	urls := []struct {
		label string
		url   string
	}{
		{"page 1 (bare)", BaseURL + "/" + user + "/films/"},
		{"page 1 (explicit)", BaseURL + "/" + user + "/films/page/1/"},
		{"page 2", BaseURL + "/" + user + "/films/page/2/"},
		{"page 3", BaseURL + "/" + user + "/films/page/3/"},
		{"page 20", BaseURL + "/" + user + "/films/page/20/"},
		{"page 35", BaseURL + "/" + user + "/films/page/35/"},
		{"page 36", BaseURL + "/" + user + "/films/page/36/"},
		{"page 37 (past end)", BaseURL + "/" + user + "/films/page/37/"},
	}

	for _, u := range urls {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		start := time.Now()
		body, err := c.Get(ctx, u.url)
		cancel()

		if err != nil {
			t.Errorf("%-18s FAILED after %s: %v", u.label, time.Since(start).Round(time.Millisecond), err)
			continue
		}
		entries, perr := ParseFilmsGrid(body)
		if perr != nil {
			t.Errorf("%-18s parse error: %v", u.label, perr)
			continue
		}
		t.Logf("%-18s ok  %6d bytes  %2d films  %s",
			u.label, len(body), len(entries), time.Since(start).Round(time.Millisecond))

		// Deliberate pacing between probes.
		time.Sleep(2 * time.Second)
	}

	requests, retries, blocks, switches := c.Snapshot()
	t.Logf("http: requests=%d retries=%d blocks=%d profile_switches=%d",
		requests, retries, blocks, switches)
}
