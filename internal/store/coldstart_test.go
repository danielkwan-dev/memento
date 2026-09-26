package store

import (
	"context"
	"testing"
	"time"
)

// A NOTIFY sent while no worker is listening is lost forever: Postgres does not
// queue notifications for disconnected listeners. On free-tier hosting the worker
// is asleep most of the time, so that is the normal case, not an edge case.
//
// The design that saves us is ordering: the worker drains the queue on startup
// BEFORE it waits on LISTEN. This test pins that down -- a job enqueued while
// nothing was listening must still be claimable.
func TestColdStart_JobEnqueuedWhileWorkerAsleepIsStillClaimed(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	// No listener exists yet: this is the worker being scaled to zero.
	job, err := st.EnqueueJob(ctx, "alice", KindScrape)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The worker now boots and drains before subscribing.
	claimed, err := st.ClaimJob(ctx, "worker-just-booted")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("a job enqueued while the worker was asleep was not claimed on startup; " +
			"the startup drain must run before waiting on LISTEN")
	}
	if claimed.ID != job.ID {
		t.Errorf("claimed %v, want %v", claimed.ID, job.ID)
	}
}

// A job starts in 'waking' so the UI can honestly say the server is booting,
// and moves to 'resolve' only once a worker actually has it.
func TestColdStart_PhaseProgressionFromWaking(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	job, err := st.EnqueueJob(ctx, "alice", KindScrape)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if job.Phase != PhaseWaking {
		t.Errorf("phase on enqueue = %s, want %s", job.Phase, PhaseWaking)
	}
	if job.Status != StatusQueued {
		t.Errorf("status on enqueue = %s, want %s", job.Status, StatusQueued)
	}

	claimed, err := st.ClaimJob(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Phase != PhaseResolve {
		t.Errorf("phase after claim = %s, want %s", claimed.Phase, PhaseResolve)
	}
	if claimed.StartedAt == nil {
		t.Error("started_at was not set on claim")
	}
}

// An SSE client that connects mid-job must be able to recover the current state
// from the database, because it missed every NOTIFY sent before it subscribed.
func TestColdStart_ProgressIsRecoverableAfterReconnect(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	job, err := st.EnqueueJob(ctx, "alice", KindScrape)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimJob(ctx, "worker-1"); err != nil {
		t.Fatal(err)
	}

	// Progress published with nobody listening: these notifications are gone.
	if err := st.UpdateProgress(ctx, job.ID, PhaseHydrate, 40, 100, 12); err != nil {
		t.Fatal(err)
	}

	// A late client reads state instead, which is why UpdateProgress writes to
	// the row as well as publishing.
	recovered, err := st.JobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil {
		// Another process claimed and finished the job. That only happens when a
		// real worker is polling the same database as the tests.
		t.Fatal("job disappeared; is a worker running against the test database?")
	}
	if recovered.Phase != PhaseHydrate {
		t.Errorf("phase = %s, want hydrate", recovered.Phase)
	}
	if recovered.FilmsDone != 40 || recovered.FilmsTotal != 100 {
		t.Errorf("progress = %d/%d, want 40/100", recovered.FilmsDone, recovered.FilmsTotal)
	}
	if recovered.CacheHits != 12 {
		t.Errorf("cache_hits = %d, want 12", recovered.CacheHits)
	}
}

// A worker killed mid-job (a scale-to-zero stop, a deploy, a crash) must not
// leave the job stuck: the reaper returns it to 'waking' for the next worker.
func TestColdStart_InterruptedJobReturnsToQueue(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	job, err := st.EnqueueJob(ctx, "alice", KindScrape)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimJob(ctx, "doomed-worker"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateProgress(ctx, job.ID, PhaseHydrate, 5, 50, 0); err != nil {
		t.Fatal(err)
	}

	// The machine vanished without finishing or failing the job.
	if _, err := st.pool.Exec(ctx,
		`UPDATE jobs SET locked_at = now() - interval '10 minutes' WHERE id = $1`,
		job.ID); err != nil {
		t.Fatal(err)
	}

	requeued, failed, err := st.ReapStaleJobs(ctx, 5*time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 1 || failed != 0 {
		t.Fatalf("requeued/failed = %d/%d, want 1/0", requeued, failed)
	}

	// It must be claimable again, and back in 'waking' since the next worker may
	// also be cold.
	again, err := st.ClaimJob(ctx, "fresh-worker")
	if err != nil {
		t.Fatal(err)
	}
	if again == nil || again.ID != job.ID {
		t.Fatalf("requeued job was not re-claimable: %v", again)
	}
	if again.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", again.Attempts)
	}
}
