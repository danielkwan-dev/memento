-- Films are GLOBAL: scraped once, shared across all users.
-- Split fetch timestamps: metadata is near-immutable, popularity stats drift.
CREATE TABLE films (
    slug                TEXT PRIMARY KEY,
    title               TEXT NOT NULL,
    year                INT,
    runtime_min         INT,
    avg_rating          NUMERIC(3,2),
    rating_count        BIGINT,
    watch_count         BIGINT,
    like_count          BIGINT,
    details_fetched_at  TIMESTAMPTZ,
    stats_fetched_at    TIMESTAMPTZ,
    fetch_version       INT NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Drives the cache-miss diff in the hydrate phase.
CREATE INDEX films_details_stale_idx ON films (details_fetched_at, fetch_version);
CREATE INDEX films_stats_stale_idx   ON films (stats_fetched_at);
-- Obscurity percentiles rank watch_count across the whole corpus.
CREATE INDEX films_watch_count_idx   ON films (watch_count) WHERE watch_count IS NOT NULL;

CREATE TABLE film_genres (
    film_slug TEXT NOT NULL REFERENCES films(slug) ON DELETE CASCADE,
    genre     TEXT NOT NULL,
    PRIMARY KEY (film_slug, genre)
);
CREATE INDEX film_genres_genre_idx ON film_genres (genre);

CREATE TABLE film_themes (
    film_slug TEXT NOT NULL REFERENCES films(slug) ON DELETE CASCADE,
    theme     TEXT NOT NULL,
    PRIMARY KEY (film_slug, theme)
);
CREATE INDEX film_themes_theme_idx ON film_themes (theme);

CREATE TYPE person_role AS ENUM ('director', 'actor');

CREATE TABLE film_people (
    film_slug     TEXT NOT NULL REFERENCES films(slug) ON DELETE CASCADE,
    person_slug   TEXT NOT NULL,
    name          TEXT NOT NULL,
    role          person_role NOT NULL,
    billing_order INT,
    PRIMARY KEY (film_slug, person_slug, role)
);
CREATE INDEX film_people_lookup_idx ON film_people (role, person_slug);

CREATE TABLE film_studios (
    film_slug TEXT NOT NULL REFERENCES films(slug) ON DELETE CASCADE,
    studio    TEXT NOT NULL,
    PRIMARY KEY (film_slug, studio)
);
CREATE INDEX film_studios_studio_idx ON film_studios (studio);

CREATE TABLE film_countries (
    film_slug TEXT NOT NULL REFERENCES films(slug) ON DELETE CASCADE,
    country   TEXT NOT NULL,
    PRIMARY KEY (film_slug, country)
);
CREATE INDEX film_countries_country_idx ON film_countries (country);

CREATE TABLE film_languages (
    film_slug TEXT NOT NULL REFERENCES films(slug) ON DELETE CASCADE,
    language  TEXT NOT NULL,
    PRIMARY KEY (film_slug, language)
);
CREATE INDEX film_languages_language_idx ON film_languages (language);

-- Personal data below. Deletable per-user without touching the film cache.
CREATE TABLE users (
    id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    username           TEXT NOT NULL UNIQUE,
    last_synced_at     TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The films grid: everything logged, no dates. One row per (user, film).
CREATE TABLE watch_entries (
    user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    film_slug TEXT   NOT NULL REFERENCES films(slug) ON DELETE CASCADE,
    rating    NUMERIC(2,1) CHECK (rating IS NULL OR (rating >= 0.5 AND rating <= 5.0)),
    liked     BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (user_id, film_slug)
);
CREATE INDEX watch_entries_user_idx ON watch_entries (user_id);

-- The diary: dated, and a film may appear many times via rewatches.
CREATE TABLE diary_entries (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    film_slug  TEXT   NOT NULL REFERENCES films(slug) ON DELETE CASCADE,
    watched_on DATE   NOT NULL,
    rating     NUMERIC(2,1) CHECK (rating IS NULL OR (rating >= 0.5 AND rating <= 5.0)),
    rewatch    BOOLEAN NOT NULL DEFAULT false,
    UNIQUE (user_id, film_slug, watched_on)
);
CREATE INDEX diary_entries_user_date_idx ON diary_entries (user_id, watched_on);

CREATE TYPE job_kind   AS ENUM ('scrape', 'import');
CREATE TYPE job_status AS ENUM ('queued', 'running', 'succeeded', 'failed', 'cancelled');
-- 'waking' is a real phase: on free-tier hosting the worker machine may be
-- scaled to zero when a job is enqueued.
CREATE TYPE job_phase  AS ENUM ('waking', 'resolve', 'index', 'hydrate', 'persist', 'done');

CREATE TABLE jobs (
    id            UUID PRIMARY KEY,
    username      TEXT       NOT NULL,
    kind          job_kind   NOT NULL,
    status        job_status NOT NULL DEFAULT 'queued',
    phase         job_phase  NOT NULL DEFAULT 'waking',
    films_total   INT        NOT NULL DEFAULT 0,
    films_done    INT        NOT NULL DEFAULT 0,
    cache_hits    INT        NOT NULL DEFAULT 0,
    error         TEXT,
    -- Queue bookkeeping for SELECT ... FOR UPDATE SKIP LOCKED.
    attempts      INT        NOT NULL DEFAULT 0,
    locked_at     TIMESTAMPTZ,
    locked_by     TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ
);

-- The queue: workers claim the oldest unlocked queued job.
CREATE INDEX jobs_queue_idx ON jobs (created_at) WHERE status = 'queued';
-- Idempotency: at most one active job per username. Replaces a Redis SET NX
-- lock with a real database constraint.
CREATE UNIQUE INDEX jobs_one_active_per_user_idx
    ON jobs (lower(username)) WHERE status IN ('queued', 'running');
-- Reaper: find jobs whose worker died mid-run.
CREATE INDEX jobs_stale_lock_idx ON jobs (locked_at) WHERE status = 'running';

-- Computed chart payloads, invalidated on re-sync.
CREATE TABLE user_stats (
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    category    TEXT   NOT NULL,
    payload     JSONB  NOT NULL,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, category)
);
