package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrJobInFlight is returned when a job for this username is already queued or
// running. The caller should hand back the existing job rather than start a
// duplicate scrape.
var ErrJobInFlight = errors.New("store: a job for this user is already in flight")

type JobStatus string

const (
	StatusQueued    JobStatus = "queued"
	StatusRunning   JobStatus = "running"
	StatusSucceeded JobStatus = "succeeded"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
)

type JobPhase string

const (
	// PhaseWaking is the starting phase: on free-tier hosting the worker machine
	// may be scaled to zero when the job is enqueued, so "waiting for a worker"
	// is a real state the UI must be able to show honestly.
	PhaseWaking  JobPhase = "waking"
	PhaseResolve JobPhase = "resolve"
	PhaseIndex   JobPhase = "index"
	PhaseHydrate JobPhase = "hydrate"
	PhasePersist JobPhase = "persist"
	PhaseDone    JobPhase = "done"
)

type JobKind string

const (
	KindScrape JobKind = "scrape"
	KindImport JobKind = "import"
)

type Job struct {
	ID         uuid.UUID `json:"id"`
	Username   string    `json:"username"`
	Kind       JobKind   `json:"kind"`
	Status     JobStatus `json:"status"`
	Phase      JobPhase  `json:"phase"`
	FilmsTotal int       `json:"films_total"`
	FilmsDone  int       `json:"films_done"`
	CacheHits  int       `json:"cache_hits"`
	Error      string    `json:"error,omitempty"`
	// Blocked marks a failure caused by Letterboxd refusing to serve this host,
	// as opposed to any other error. The UI offers the export upload on this.
	Blocked    bool       `json:"blocked"`
	Attempts   int        `json:"attempts"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Terminal reports whether the job has finished, successfully or not.
func (j *Job) Terminal() bool {
	return j.Status == StatusSucceeded || j.Status == StatusFailed || j.Status == StatusCancelled
}

const jobColumns = `id, username, kind, status, phase, films_total, films_done,
	cache_hits, COALESCE(error, ''), blocked, attempts, created_at, started_at,
	finished_at`

// jobColumnsQualified is jobColumns with every column prefixed by the "j" alias.
// UPDATE ... FROM brings the CTE's columns into scope, so an unqualified "id" in
// RETURNING is ambiguous between the target table and the CTE.
const jobColumnsQualified = `j.id, j.username, j.kind, j.status, j.phase,
	j.films_total, j.films_done, j.cache_hits, COALESCE(j.error, ''), j.blocked,
	j.attempts, j.created_at, j.started_at, j.finished_at`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Username, &j.Kind, &j.Status, &j.Phase,
		&j.FilmsTotal, &j.FilmsDone, &j.CacheHits, &j.Error, &j.Blocked,
		&j.Attempts, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// EnqueueJob creates a queued job. The unique partial index on active jobs makes
// this idempotent per username: hammering the sync button cannot fan out into
// duplicate scrapes, and the constraint lives in the database rather than in an
// advisory lock that a crashed worker could leak.
func (s *Store) EnqueueJob(ctx context.Context, username string, kind JobKind) (*Job, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	id := uuid.New()

	row := s.pool.QueryRow(ctx, `
		INSERT INTO jobs (id, username, kind, status, phase)
		VALUES ($1, $2, $3, 'queued', 'waking')
		RETURNING `+jobColumns, id, username, string(kind))

	j, err := scanJob(row)
	if err != nil {
		var pgErr *pgconn.PgError
		// 23505 = unique_violation on jobs_one_active_per_user_idx.
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrJobInFlight
		}
		return nil, fmt.Errorf("enqueue job: %w", err)
	}
	// Wake any worker that is already listening.
	if err := s.notifyJobAvailable(ctx); err != nil {
		return nil, err
	}
	return j, nil
}

// EnqueueImportJob creates a queued import job carrying its export archive.
//
// The payload rides along in the jobs row: it is small, needed once by whichever
// worker claims the job, and deleting the job deletes it -- no object storage,
// credentials or cleanup job required.
func (s *Store) EnqueueImportJob(ctx context.Context, username string, payload []byte) (*Job, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	id := uuid.New()

	row := s.pool.QueryRow(ctx, `
		INSERT INTO jobs (id, username, kind, status, phase, payload)
		VALUES ($1, $2, 'import', 'queued', 'waking', $3)
		RETURNING `+jobColumns, id, username, payload)

	j, err := scanJob(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrJobInFlight
		}
		return nil, fmt.Errorf("enqueue import job: %w", err)
	}
	if err := s.notifyJobAvailable(ctx); err != nil {
		return nil, err
	}
	return j, nil
}

// JobPayload reads an import job's archive. Kept off the Job struct so ordinary
// job reads and progress notifications never carry megabytes of CSV.
func (s *Store) JobPayload(ctx context.Context, id uuid.UUID) ([]byte, error) {
	var payload []byte
	err := s.pool.QueryRow(ctx, `SELECT payload FROM jobs WHERE id = $1`, id).Scan(&payload)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("job payload: %w", err)
	}
	return payload, nil
}

// ClearJobPayload drops the archive once the import has finished, so a finished
// job does not keep holding the upload.
func (s *Store) ClearJobPayload(ctx context.Context, id uuid.UUID) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE jobs SET payload = NULL WHERE id = $1`, id); err != nil {
		return fmt.Errorf("clear job payload: %w", err)
	}
	return nil
}

// ActiveJobFor returns the in-flight job for a username, if any.
func (s *Store) ActiveJobFor(ctx context.Context, username string) (*Job, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+jobColumns+` FROM jobs
		WHERE username = $1 AND status IN ('queued', 'running')
		ORDER BY created_at DESC LIMIT 1`,
		strings.ToLower(strings.TrimSpace(username)))
	j, err := scanJob(row)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("active job: %w", err)
	}
	return j, nil
}

func (s *Store) JobByID(ctx context.Context, id uuid.UUID) (*Job, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id)
	j, err := scanJob(row)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("job by id: %w", err)
	}
	return j, nil
}

// ClaimJob atomically takes the oldest queued job.
//
// FOR UPDATE SKIP LOCKED is what lets several workers poll the same table
// without blocking each other or handing the same job to two of them, which is
// why this needs no external queue.
func (s *Store) ClaimJob(ctx context.Context, workerID string) (*Job, error) {
	row := s.pool.QueryRow(ctx, `
		WITH claimed AS (
			SELECT id FROM jobs
			WHERE status = 'queued'
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE jobs j
		SET status = 'running', phase = 'resolve', attempts = j.attempts + 1,
		    locked_at = now(), locked_by = $1,
		    started_at = COALESCE(j.started_at, now())
		FROM claimed
		WHERE j.id = claimed.id
		RETURNING `+jobColumnsQualified, workerID)

	j, err := scanJob(row)
	if err != nil {
		if isNoRows(err) {
			return nil, nil // nothing queued
		}
		return nil, fmt.Errorf("claim job: %w", err)
	}
	return j, nil
}

// JobProgress is the payload published on the job's notification channel and
// mirrored into the jobs row.
type JobProgress struct {
	JobID      uuid.UUID `json:"job_id"`
	Status     JobStatus `json:"status"`
	Phase      JobPhase  `json:"phase"`
	FilmsTotal int       `json:"films_total"`
	FilmsDone  int       `json:"films_done"`
	CacheHits  int       `json:"cache_hits"`
	Error      string    `json:"error,omitempty"`
	Blocked    bool      `json:"blocked,omitempty"`
}

// UpdateProgress persists progress and publishes it to any SSE subscriber.
//
// Postgres is the durable record so a client reconnecting mid-job can recover
// state; NOTIFY is the ephemeral fan-out so it sees live ticks without polling.
func (s *Store) UpdateProgress(ctx context.Context, id uuid.UUID, phase JobPhase, done, total, cacheHits int) error {
	row := s.pool.QueryRow(ctx, `
		UPDATE jobs SET phase = $2, films_done = $3, films_total = $4,
		                cache_hits = $5, locked_at = now()
		WHERE id = $1
		RETURNING `+jobColumns, id, string(phase), done, total, cacheHits)
	j, err := scanJob(row)
	if err != nil {
		return fmt.Errorf("update progress: %w", err)
	}
	return s.publishProgress(ctx, j)
}

// FinishJob marks a job terminal. A non-empty errMsg means failure; blocked marks
// the specific case of Letterboxd refusing to serve this host.
func (s *Store) FinishJob(ctx context.Context, id uuid.UUID, errMsg string, blocked bool) error {
	status := StatusSucceeded
	phase := PhaseDone
	if errMsg != "" {
		status = StatusFailed
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE jobs SET status = $2, phase = $3, error = NULLIF($4, ''),
		                blocked = $5, finished_at = now(), locked_at = NULL
		WHERE id = $1
		RETURNING `+jobColumns, id, string(status), string(phase), errMsg, blocked)
	j, err := scanJob(row)
	if err != nil {
		return fmt.Errorf("finish job: %w", err)
	}
	return s.publishProgress(ctx, j)
}

// CancelJob marks an active job cancelled, freeing the per-username slot so the
// user can start another. Returns false if the job was already terminal.
func (s *Store) CancelJob(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'cancelled', phase = 'done', finished_at = now(),
		    locked_at = NULL, error = COALESCE(error, 'cancelled by the user')
		WHERE id = $1 AND status IN ('queued', 'running')`, id)
	if err != nil {
		return false, fmt.Errorf("cancel job: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ReapStaleJobs requeues jobs whose worker died mid-run, so a crash or a
// scaled-to-zero machine does not leave a user's job stuck forever. Jobs that
// have already burned through maxAttempts are failed instead of retried.
func (s *Store) ReapStaleJobs(ctx context.Context, staleAfter time.Duration, maxAttempts int) (requeued, failed int, err error) {
	if err := s.pool.QueryRow(ctx, `
		WITH stale AS (
			SELECT id FROM jobs
			WHERE status = 'running' AND locked_at < now() - $1::interval
		), retried AS (
			UPDATE jobs SET status = 'queued', phase = 'waking', locked_at = NULL, locked_by = NULL
			WHERE id IN (SELECT id FROM stale) AND attempts < $2
			RETURNING 1
		)
		SELECT count(*) FROM retried`, staleAfter, maxAttempts).Scan(&requeued); err != nil {
		return 0, 0, fmt.Errorf("requeue stale jobs: %w", err)
	}

	if err := s.pool.QueryRow(ctx, `
		WITH dead AS (
			UPDATE jobs SET status = 'failed', error = 'worker died and retries exhausted',
			                finished_at = now(), locked_at = NULL
			WHERE status = 'running' AND locked_at < now() - $1::interval AND attempts >= $2
			RETURNING 1
		)
		SELECT count(*) FROM dead`, staleAfter, maxAttempts).Scan(&failed); err != nil {
		return requeued, 0, fmt.Errorf("fail exhausted jobs: %w", err)
	}
	return requeued, failed, nil
}

// --- LISTEN/NOTIFY plumbing ---

// JobChannel is the notification channel for one job's progress. Per-job
// channels mean an SSE connection is woken only by its own job.
func JobChannel(id uuid.UUID) string {
	// Hyphens are not valid unquoted identifiers in LISTEN.
	return "job_" + strings.ReplaceAll(id.String(), "-", "_")
}

// QueueChannel wakes idle workers the moment a job is enqueued, so they need
// not poll tightly.
const QueueChannel = "memento_job_queued"

func (s *Store) notifyJobAvailable(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `SELECT pg_notify($1, '')`, QueueChannel); err != nil {
		return fmt.Errorf("notify queue: %w", err)
	}
	return nil
}

func (s *Store) publishProgress(ctx context.Context, j *Job) error {
	payload, err := json.Marshal(JobProgress{
		JobID:      j.ID,
		Status:     j.Status,
		Phase:      j.Phase,
		FilmsTotal: j.FilmsTotal,
		FilmsDone:  j.FilmsDone,
		CacheHits:  j.CacheHits,
		Error:      j.Error,
		Blocked:    j.Blocked,
	})
	if err != nil {
		return fmt.Errorf("marshal progress: %w", err)
	}
	// NOTIFY payloads are capped at 8000 bytes; a progress tick is far smaller.
	if _, err := s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`,
		JobChannel(j.ID), string(payload)); err != nil {
		return fmt.Errorf("notify progress: %w", err)
	}
	return nil
}

// Listen subscribes to a Postgres notification channel and delivers payloads on
// the returned channel until ctx is cancelled. It holds a dedicated connection,
// because LISTEN is connection-scoped and must not be returned to the pool.
func (s *Store) Listen(ctx context.Context, channel string) (<-chan string, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire listener conn: %w", err)
	}
	if _, err := conn.Exec(ctx, `LISTEN `+pgx.Identifier{channel}.Sanitize()); err != nil {
		conn.Release()
		return nil, fmt.Errorf("listen %s: %w", channel, err)
	}

	out := make(chan string, 16)
	go func() {
		defer close(out)
		defer conn.Release()
		for {
			n, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				return // ctx cancelled or connection lost
			}
			select {
			case out <- n.Payload:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
