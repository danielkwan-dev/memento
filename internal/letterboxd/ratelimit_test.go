package letterboxd

import (
	"errors"
	"testing"
	"time"
)

// A 429 is about volume, not identity. Measured against the live site: once a 429
// starts, EVERY TLS profile gets one -- five profiles succeeded, then the next ten
// all returned 429 regardless of which browser they impersonated. So it must be
// classified apart from a 403, which is about identity and where rotating helps.
func TestRateLimitError_IsDistinctFromBlocked(t *testing.T) {
	err := &rateLimitError{}

	if !errors.Is(err, ErrRateLimited) {
		t.Error("rateLimitError must unwrap to ErrRateLimited")
	}
	if errors.Is(err, ErrBlocked) {
		t.Error("a 429 must NOT be treated as ErrBlocked; rotating profiles cannot help")
	}
}

func TestRateLimitError_CarriesRetryAfter(t *testing.T) {
	err := &rateLimitError{retryAfter: 30 * time.Second}
	if !contains(err.Error(), "30s") {
		t.Errorf("error should mention the delay, got %q", err.Error())
	}

	var target *rateLimitError
	if !errors.As(error(err), &target) {
		t.Fatal("errors.As failed to extract rateLimitError")
	}
	if target.retryAfter != 30*time.Second {
		t.Errorf("retryAfter = %v, want 30s", target.retryAfter)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"30":      30 * time.Second,
		"1":       time.Second,
		"  45  ":  45 * time.Second,
		"":        0,
		"garbage": 0,
		"-5":      0,
		"0":       0,
		// Capped, so a server asking for an hour cannot wedge a job that long.
		"99999": 120 * time.Second,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

// Rate limiting must back off far harder than a transient error.
func TestConfig_RateLimitBackoffIsLongest(t *testing.T) {
	c := DefaultConfig()
	if c.RateLimitBackoff <= c.BlockedBackoff {
		t.Errorf("RateLimitBackoff (%v) should exceed BlockedBackoff (%v)",
			c.RateLimitBackoff, c.BlockedBackoff)
	}
	if c.BlockedBackoff <= c.TransientBackoff {
		t.Errorf("BlockedBackoff (%v) should exceed TransientBackoff (%v)",
			c.BlockedBackoff, c.TransientBackoff)
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
