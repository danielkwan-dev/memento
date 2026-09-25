package letterboxd

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLiveDetailThroughput measures whether film detail pages really tolerate
// high concurrency.
//
// The reference project assumes they do, on the grounds that film pages sit in
// Cloudflare's edge cache while user pages do not. This checks that claim against
// the live site at several concurrency levels, and reports blocks and retries so
// the DetailConcurrency default is a measurement rather than an assumption.
//
//	MEMENTO_LIVE=1 go test ./internal/letterboxd/ -run TestLiveDetailThroughput -v
func TestLiveDetailThroughput(t *testing.T) {
	if os.Getenv("MEMENTO_LIVE") != "1" {
		t.Skip("set MEMENTO_LIVE=1 to run live network tests")
	}

	// A fixed set of well-known films: all certainly edge-cached, so this
	// measures the gate rather than cache misses.
	slugs := []string{
		"parasite-2019", "whiplash-2014", "the-general",
		"everything-everywhere-all-at-once", "past-lives",
		"la-la-land", "arrival-2016", "dune-2021",
		"the-substance", "anora", "challengers", "nosferatu-2024",
	}

	for _, concurrency := range []int{4, 8, 16} {
		t.Run(name(concurrency), func(t *testing.T) {
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			c, err := NewClient(DefaultConfig(), log)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			var (
				wg       sync.WaitGroup
				ok       atomic.Int64
				failed   atomic.Int64
				sem      = make(chan struct{}, concurrency)
				start    = time.Now()
				firstErr atomic.Value
			)

			for _, slug := range slugs {
				slug := slug
				wg.Add(1)
				go func() {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					if _, err := c.Get(ctx, BaseURL+"/film/"+slug+"/"); err != nil {
						failed.Add(1)
						firstErr.CompareAndSwap(nil, err.Error())
						return
					}
					ok.Add(1)
				}()
			}
			wg.Wait()

			elapsed := time.Since(start)
			requests, retries, blocks, switches := c.Snapshot()
			t.Logf("concurrency=%2d  ok=%2d failed=%2d  %s  (requests=%d retries=%d blocks=%d switches=%d)",
				concurrency, ok.Load(), failed.Load(), elapsed.Round(time.Millisecond),
				requests, retries, blocks, switches)
			if e := firstErr.Load(); e != nil {
				t.Logf("  first error: %v", e)
			}

			if failed.Load() > 0 {
				t.Errorf("concurrency %d: %d of %d requests failed", concurrency, failed.Load(), len(slugs))
			}
		})

		// Cool off between levels so one run does not poison the next.
		time.Sleep(10 * time.Second)
	}
}

func name(n int) string {
	return "concurrency=" + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
