package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitFor polls until cond holds or the deadline passes, so the tests do not
// depend on a fixed sleep.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func TestWaker_TouchProbesWorker(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := NewWaker(srv.URL+"/healthz", quietLogger())
	if !w.Enabled() {
		t.Fatal("Enabled() = false, want true when a URL is configured")
	}

	w.Touch()
	if !waitFor(t, 2*time.Second, func() bool { return hits.Load() == 1 }) {
		t.Fatalf("probe count = %d, want 1", hits.Load())
	}
}

// Touch must be throttled: the frontend calls /healthz on every page load, and a
// cold boot takes seconds, so probing repeatedly cannot help.
func TestWaker_TouchIsThrottled(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	w := NewWaker(srv.URL+"/healthz", quietLogger())
	for i := 0; i < 20; i++ {
		w.Touch()
	}

	// Let the first probe land, then confirm the rest were suppressed.
	if !waitFor(t, 2*time.Second, func() bool { return hits.Load() >= 1 }) {
		t.Fatal("no probe was sent")
	}
	time.Sleep(100 * time.Millisecond)
	if got := hits.Load(); got != 1 {
		t.Errorf("probe count = %d, want exactly 1 (throttled)", got)
	}
}

// A disabled waker must be a no-op, which is the local-development case.
func TestWaker_DisabledWithoutURL(t *testing.T) {
	w := NewWaker("", quietLogger())
	if w.Enabled() {
		t.Error("Enabled() = true, want false with no URL")
	}
	w.Touch() // must not panic
}

// A cold machine refuses connections at first. That must be tolerated quietly:
// the worker also polls the queue, so a failed wake delays a job, never loses it.
func TestWaker_TolerateUnreachableWorker(t *testing.T) {
	// Port 1 on localhost reliably refuses connections.
	w := NewWaker("http://127.0.0.1:1/healthz", quietLogger())
	w.Touch()
	time.Sleep(150 * time.Millisecond)
	// Reaching here without a panic is the assertion; a second Touch must still
	// be throttled rather than spinning.
	w.Touch()
}

// The probe outlives the request that triggered it: it must not be tied to a
// request context that is cancelled when the response is written.
func TestWaker_ProbeSurvivesRequestCompletion(t *testing.T) {
	release := make(chan struct{})
	var completed atomic.Bool

	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the probe open past the API response
		completed.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer worker.Close()

	waker := NewWaker(worker.URL+"/healthz", quietLogger())

	// Simulate an API handler that touches the waker then returns immediately.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		waker.Touch()
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	resp, err := http.Get(api.URL)
	if err != nil {
		t.Fatalf("api request: %v", err)
	}
	_ = resp.Body.Close()

	// The API response is done; the probe must still be in flight.
	if completed.Load() {
		t.Fatal("probe finished before it was released")
	}
	close(release)
	if !waitFor(t, 2*time.Second, func() bool { return completed.Load() }) {
		t.Error("probe did not complete after the API request finished")
	}
}
