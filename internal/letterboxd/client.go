package letterboxd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const BaseURL = "https://letterboxd.com"

// Fetcher is the seam between the network and everything above it. The pipeline
// and parsers depend only on this, so they are tested with a fake and never
// need TLS, Cloudflare or the network.
type Fetcher interface {
	Get(ctx context.Context, url string) ([]byte, error)
}

// browserProfiles are tried in order when we start getting blocked. Letterboxd
// sits behind Cloudflare, which fingerprints the TLS ClientHello and the HTTP/2
// frame ordering; Go's default net/http is trivially identifiable. tls-client
// replays real browser fingerprints instead.
var browserProfiles = []struct {
	name    string
	profile profiles.ClientProfile
}{
	{"chrome_152", profiles.Chrome_152_PSK},
	{"chrome_150", profiles.Chrome_150},
	{"firefox_148", profiles.Firefox_148},
	{"chrome_146", profiles.Chrome_146},
	{"safari_16_0", profiles.Safari_16_0},
}

// Config tunes retry and backoff behaviour.
type Config struct {
	MaxAttempts      int
	TransientBackoff time.Duration
	BlockedBackoff   time.Duration
	// RateLimitBackoff applies to 429s, which need far longer than a block.
	RateLimitBackoff time.Duration
	MaxBackoff       time.Duration
	RequestTimeout   time.Duration
	// BlockStreakToSwitch is how many consecutive blocks (with no success in
	// between) force a profile rotation. Requiring a *streak* stops us flapping
	// between profiles that both work fine when a block is just noise.
	//
	// Each individual retry already walks to a different profile, so this governs
	// only the starting point for NEW requests. The reference implementation
	// promotes after 5 consecutive blocks; matching that means a fingerprint that
	// has genuinely gone stale stops being tried first.
	BlockStreakToSwitch int
}

func DefaultConfig() Config {
	return Config{
		MaxAttempts:      6,
		TransientBackoff: 1500 * time.Millisecond,
		// Blocks are usually rate limiting that clears on its own, so start the
		// backoff high and let it grow: waiting is what actually recovers.
		BlockedBackoff: 8 * time.Second,
		// A 429 outlasts a short ladder; starting high beats three wasted tries.
		RateLimitBackoff:    20 * time.Second,
		MaxBackoff:          90 * time.Second,
		RequestTimeout:      30 * time.Second,
		BlockStreakToSwitch: 5,
	}
}

// Client is a Fetcher that impersonates real browsers and retries with
// classified backoff. Safe for concurrent use.
type Client struct {
	cfg Config
	log *slog.Logger

	mu          sync.Mutex
	clients     map[string]tls_client.HttpClient
	profileIdx  int
	blockStreak int

	stats Stats
}

// Stats reports how hard a scrape had to work. Surfaced in logs and job output.
type Stats struct {
	mu            sync.Mutex
	Requests      int64
	Retries       int64
	Blocks        int64
	ProfileSwitch int64
}

func (s *Stats) snapshot() (requests, retries, blocks, switches int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Requests, s.Retries, s.Blocks, s.ProfileSwitch
}

func (s *Stats) add(requests, retries, blocks, switches int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests += requests
	s.Retries += retries
	s.Blocks += blocks
	s.ProfileSwitch += switches
}

func NewClient(cfg Config, log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	c := &Client{
		cfg:     cfg,
		log:     log,
		clients: make(map[string]tls_client.HttpClient, len(browserProfiles)),
	}
	// Build one underlying client per profile up front so rotation is free.
	for _, bp := range browserProfiles {
		hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
			tls_client.WithClientProfile(bp.profile),
			tls_client.WithTimeoutSeconds(int(cfg.RequestTimeout.Seconds())),
			tls_client.WithCookieJar(tls_client.NewCookieJar()),
		)
		if err != nil {
			return nil, fmt.Errorf("build tls client for %s: %w", bp.name, err)
		}
		c.clients[bp.name] = hc
	}
	return c, nil
}

// Snapshot returns the request counters so a finished job can report effort.
func (c *Client) Snapshot() (requests, retries, blocks, switches int64) {
	return c.stats.snapshot()
}

func (c *Client) current() (string, tls_client.HttpClient) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bp := browserProfiles[c.profileIdx]
	return bp.name, c.clients[bp.name]
}

// preferredIdx is the profile a fresh request starts from.
func (c *Client) preferredIdx() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.profileIdx
}

// noteResult drives profile rotation. Only a streak of blocks with no
// intervening success switches the default profile.
//
// It also discards the cookie jars on rotation, which matters more than the
// rotation itself: a failed Cloudflare challenge leaves a cookie marking the
// session as suspect, and a long-lived client keeps presenting it on every
// subsequent request. Observed locally -- a freshly built client fetched a profile
// fine while the worker's 10-minute-old client was blocked on the same URL, six
// attempts in a row. Without this the client never recovers.
func (c *Client) noteResult(blocked bool) (switched bool, newProfile string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !blocked {
		c.blockStreak = 0
		return false, ""
	}
	c.blockStreak++
	if c.blockStreak < c.cfg.BlockStreakToSwitch {
		return false, ""
	}
	c.blockStreak = 0
	c.profileIdx = (c.profileIdx + 1) % len(browserProfiles)
	c.resetSessionsLocked()
	return true, browserProfiles[c.profileIdx].name
}

// resetSessionsLocked rebuilds every underlying client, dropping cookies and open
// connections. The caller must hold c.mu.
func (c *Client) resetSessionsLocked() {
	for _, bp := range browserProfiles {
		hc, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
			tls_client.WithClientProfile(bp.profile),
			tls_client.WithTimeoutSeconds(int(c.cfg.RequestTimeout.Seconds())),
			tls_client.WithCookieJar(tls_client.NewCookieJar()),
		)
		if err != nil {
			// Keep the existing client rather than leaving a nil in the map.
			continue
		}
		c.clients[bp.name] = hc
	}
}

// Get fetches url, retrying transient failures and Cloudflare blocks with
// classified exponential backoff. It respects ctx cancellation between and
// during attempts.
func (c *Client) Get(ctx context.Context, url string) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt < c.cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Try the preferred profile first, then the others in turn. The reference
		// implementation does the same, and it matters: retrying a blocked request
		// under the SAME fingerprint mostly just burns the retry budget, while a
		// different one often succeeds immediately.
		body, err := c.attempt(ctx, url, c.preferredIdx()+attempt)
		if err == nil {
			c.noteResult(false)
			return body, nil
		}
		lastErr = err

		// Terminal conditions: retrying cannot help.
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrPrivate) {
			return nil, err
		}

		// A 429 is about volume: every profile gets one, so rotating is pointless
		// and the only useful response is to wait longer.
		limited := errors.Is(err, ErrRateLimited)
		blocked := errors.Is(err, ErrBlocked)
		if blocked {
			c.stats.add(0, 0, 1, 0)
			if switched, name := c.noteResult(true); switched {
				c.stats.add(0, 0, 0, 1)
				c.log.Warn("rotating browser profile after block streak",
					"new_profile", name, "url", url)
			}
		} else if !limited {
			c.noteResult(false)
		}

		// Last attempt: don't sleep, just report.
		if attempt == c.cfg.MaxAttempts-1 {
			break
		}

		base := c.cfg.TransientBackoff
		switch {
		case limited:
			base = c.cfg.RateLimitBackoff
		case blocked:
			base = c.cfg.BlockedBackoff
		}
		wait := backoff(base, attempt, c.cfg.MaxBackoff)
		// The server's own Retry-After beats our guess.
		var rle *rateLimitError
		if errors.As(err, &rle) && rle.retryAfter > wait {
			wait = rle.retryAfter
		}
		c.stats.add(0, 1, 0, 0)
		c.log.Debug("retrying", "url", url, "attempt", attempt+1,
			"wait", wait, "blocked", blocked, "err", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}

	return nil, fmt.Errorf("get %s after %d attempts: %w", url, c.cfg.MaxAttempts, lastErr)
}

// backoff grows exponentially from base and adds jitter so concurrent workers
// don't retry in lockstep.
func backoff(base time.Duration, attempt int, max time.Duration) time.Duration {
	d := float64(base) * math.Pow(2, float64(attempt))
	if d > float64(max) {
		d = float64(max)
	}
	// Jitter over the top half of the window: [d/2, d).
	jittered := d/2 + rand.Float64()*(d/2)
	return time.Duration(jittered)
}

func (c *Client) attempt(ctx context.Context, url string, idx int) ([]byte, error) {
	bp := browserProfiles[idx%len(browserProfiles)]
	profileName := bp.name
	c.mu.Lock()
	hc := c.clients[profileName]
	c.mu.Unlock()

	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	setBrowserHeaders(req, profileName)

	c.stats.add(1, 0, 0, 0)

	resp, err := hc.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: %v", ErrTransient, err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == fhttp.StatusOK:
		if readErr != nil {
			return nil, fmt.Errorf("%w: read body: %v", ErrTransient, readErr)
		}
		// A 200 can still be a Cloudflare interstitial.
		if looksLikeChallenge(body) {
			return nil, ErrBlocked
		}
		return body, nil

	case resp.StatusCode == fhttp.StatusNotFound:
		return nil, ErrNotFound

	case resp.StatusCode == fhttp.StatusTooManyRequests:
		// A 429 is about volume, not identity. Honour Retry-After when present:
		// guessing shorter than the server asked for just earns another 429.
		wait := parseRetryAfter(resp.Header.Get("retry-after"))
		return nil, &rateLimitError{retryAfter: wait}

	case resp.StatusCode == fhttp.StatusForbidden,
		resp.StatusCode == fhttp.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: status %d", ErrBlocked, resp.StatusCode)

	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: status %d", ErrTransient, resp.StatusCode)

	default:
		return nil, fmt.Errorf("%w: unexpected status %d", ErrTransient, resp.StatusCode)
	}
}

// rateLimitError carries the server's requested wait, so the retry loop can
// honour it instead of guessing.
type rateLimitError struct {
	retryAfter time.Duration
}

func (e *rateLimitError) Error() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("%v (retry after %v)", ErrRateLimited, e.retryAfter)
	}
	return ErrRateLimited.Error()
}

func (e *rateLimitError) Unwrap() error { return ErrRateLimited }

// parseRetryAfter reads the header's delta-seconds form. Cloudflare also permits
// an HTTP date, which is rarer here and not worth the parsing.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	// Cap it: a server asking for an hour should not wedge a job that long.
	if n > 120 {
		n = 120
	}
	return time.Duration(n) * time.Second
}

// challengeMarkers appear in Cloudflare interstitials served with a 200.
//
// These must be strings a REAL page never contains, which is narrower than it
// sounds: Letterboxd embeds Cloudflare's Turnstile script on ordinary pages, so
// "challenge-platform" appears in perfectly good HTML. Matching on it rejected
// every successful fetch of a profile whose page happened to be small enough to
// get scanned -- a 146KB page was refused six times in a row while the identical
// request returned 200 with full content. Only the interstitial's own title and
// body text are safe to match.
var challengeMarkers = []string{
	"<title>just a moment",
	"cf-browser-verification",
	"attention required! | cloudflare",
	"enable javascript and cookies to continue",
	"sorry, you have been blocked",
}

// maxChallengeBodySize bounds the scan. A Cloudflare interstitial is a few KB;
// anything larger is a real page. This is a cheap guard, NOT the safety net --
// the markers themselves have to be unambiguous, because a real page can easily
// come in under any size threshold.
const maxChallengeBodySize = 64 * 1024

func looksLikeChallenge(body []byte) bool {
	if len(body) > maxChallengeBodySize {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, m := range challengeMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func setBrowserHeaders(req *fhttp.Request, profileName string) {
	ua := userAgents[profileName]
	if ua == "" {
		ua = userAgents["chrome_152"]
	}
	req.Header.Set("user-agent", ua)

	switch {
	case strings.HasPrefix(profileName, "firefox"):
		// Firefox sends a distinctive accept string and no client hints.
		req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("accept-language", "en-US,en;q=0.5")
		req.Header.Set("accept-encoding", "gzip, deflate, br, zstd")
		req.Header.Set("upgrade-insecure-requests", "1")
		req.Header.Set("sec-fetch-dest", "document")
		req.Header.Set("sec-fetch-mode", "navigate")
		req.Header.Set("sec-fetch-site", "none")
		req.Header.Set("sec-fetch-user", "?1")
		req.Header.Set("priority", "u=0, i")

	case strings.HasPrefix(profileName, "safari"):
		// Safari sends neither client hints nor sec-fetch-user, and its accept
		// header differs again.
		req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("accept-language", "en-US,en;q=0.9")
		req.Header.Set("accept-encoding", "gzip, deflate, br")
		req.Header.Set("sec-fetch-dest", "document")
		req.Header.Set("sec-fetch-mode", "navigate")
		req.Header.Set("sec-fetch-site", "none")

	default: // chromium
		req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
		req.Header.Set("accept-language", "en-US,en;q=0.9")
		req.Header.Set("accept-encoding", "gzip, deflate, br, zstd")
		req.Header.Set("upgrade-insecure-requests", "1")
		req.Header.Set("sec-fetch-dest", "document")
		req.Header.Set("sec-fetch-mode", "navigate")
		req.Header.Set("sec-fetch-site", "none")
		req.Header.Set("sec-fetch-user", "?1")
		req.Header.Set("priority", "u=0, i")
		if v, ok := chromeVersionHints[profileName]; ok {
			req.Header.Set("sec-ch-ua", chromeUAHint(v))
			req.Header.Set("sec-ch-ua-mobile", "?0")
			req.Header.Set("sec-ch-ua-platform", platformHint)
		}
	}
}

const platformHint = "\"Windows\""

func chromeUAHint(major string) string {
	return fmt.Sprintf("\"Chromium\";v=%q, \"Not(A:Brand\";v=\"24\", \"Google Chrome\";v=%q", major, major)
}

// userAgents must stay consistent with the TLS profile: a Chrome fingerprint
// paired with a Firefox UA string is itself a detection signal.
var userAgents = map[string]string{
	"chrome_152":  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
	"chrome_150":  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36",
	"chrome_146":  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
	"firefox_148": "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0",
	"safari_16_0": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Safari/605.1.15",
}

// chromeVersionHints maps a profile to its sec-ch-ua major version so the
// client hints match the impersonated browser.
var chromeVersionHints = map[string]string{
	"chrome_152": "152",
	"chrome_150": "150",
	"chrome_146": "146",
}
