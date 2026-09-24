# Memento

Turns a public Letterboxd profile into charts: ratings, genres, themes, decades,
directors, obscurity, runtime, geography.

Go API + worker, Postgres, React frontend. Inspired by
[rubylu-05/memento](https://github.com/rubylu-05/memento), rebuilt with a
persistent shared cache, background jobs, and live progress streaming.

## Why it is built this way

**A global film cache, not per-user storage.** Film metadata is identical for
everyone, so `films` is keyed by slug and shared across all users. A 500-film
profile needs ~1000 HTTP requests on a cold cache; once popular films are cached,
a new user with mainstream taste may need almost none. Details and popularity
stats carry separate TTLs because they age differently — genres and runtime never
really change, watch counts drift daily. Personal data (`watch_entries`,
`diary_entries`) stays per-user and cascades on delete, so the cache costs nothing
in privacy terms.

**Postgres as the queue.** `SELECT … FOR UPDATE SKIP LOCKED` lets several workers
poll one table without blocking or double-claiming; `LISTEN/NOTIFY` fans progress
out to SSE subscribers. That removes Redis entirely — one less service to deploy,
with job state durable across restarts. Idempotency is a unique partial index on
active jobs per username, so double-clicking Sync cannot start two scrapes; a
crashed worker can't leak the lock the way an advisory lock would.

**Two concurrency limits, not one.** A user's grid and diary pages are not in
Cloudflare's edge cache, so they run at 3 concurrent. Film detail pages are cached
at the edge and run at 16. Same reasoning as the reference project's split thread
pools, expressed as two `semaphore.Weighted` instances.

**TLS fingerprint spoofing.** Letterboxd rejects clients whose TLS handshake isn't
a real browser's. Go's `net/http` is trivially identifiable, so the fetcher uses
[`bogdanfinn/tls-client`](https://github.com/bogdanfinn/tls-client) (uTLS +
`fhttp`) to replay real Chrome/Firefox/Safari ClientHellos and HTTP/2 frame
ordering. The difference is measurable: `/csi/film/{slug}/stats/` returns **403**
to curl — even with a spoofed `User-Agent` — and **200** to this client.

**Blocks are usually rate limiting, not detection.** A blocked URL succeeds
minutes later on the same profile. Rotating fingerprints eagerly made it *worse*,
because it retries a hot path under a fresh identity. So rotation needs a long
block streak, and blocked requests back off harder instead.

**Failure is proportionate.** One deleted film must not abandon a 2000-film
scrape, so per-film failures are counted, not propagated. But if more than 20% of
films fail, that is a broken parser rather than missing films, and the job fails
loudly instead of storing a gutted profile.

## Cold start

The free tier scales machines to zero, so the first request after an idle period
waits for a container boot. This is handled as real application state, not a
spinner:

- `waking` is a genuine job phase in the database, shown in the UI with a
  plain-language explanation.
- The frontend probes `/healthz` on page load, so the boot overlaps the time the
  user spends reading and typing rather than following their submit.
- Waking the API does **not** wake the worker — separate machines. So the worker
  runs an HTTP health endpoint purely to be wakeable, and the API pings it on both
  `/healthz` and `/sync`. The probe is throttled and detached from the request
  context so it outlives the response.
- A `NOTIFY` sent while the worker is asleep is lost forever, so the worker drains
  the queue on startup *before* it waits on `LISTEN`.
- A worker killed mid-job leaves the job reclaimable: the reaper returns it to
  `waking` for the next worker, up to 3 attempts.

## Architecture

```
React (Vite, SSE) ──► Go API ──► Postgres ◄── Go worker ──► Letterboxd
                        │                          │
                    LISTEN/NOTIFY            4-phase pipeline
                                      resolve → index → hydrate → persist
```

`hydrate` is the interesting phase: it diffs the profile's film slugs against the
cache and fetches only what is missing or stale.

## Running it

```bash
docker compose up -d postgres         # Postgres on :5432

go run ./cmd/api                      # :8080
go run ./cmd/worker                   # :8081

cd web && npm install && npm run dev  # :5173
```

Configuration is environment-based; see `.env.example`.

## Tests

```bash
go test ./...        # parsers, pipeline, waker — all offline
npm run build        # typecheck + frontend build
```

Parser tests run against **committed HTML fixtures**, not the live site: fast,
deterministic, and offline. There is one fixture per HTML *shape* rather than per
film (every film page uses the same template), plus the edge cases that actually
break parsers — a silent film with no spoken language, a film with two directors,
a last page with no paginator, and a real Cloudflare interstitial. Refresh them
with `go run ./cmd/fixtures -all` when Letterboxd changes markup.

Pipeline tests use a fake fetcher with synthesized pages, which is how the
concurrency, caching and cancellation behaviour gets tested at scale without
committing thousands of files.

Store tests need real Postgres, because the behaviour under test *is* the SQL:

```bash
MEMENTO_TEST_DATABASE_URL=postgres://memento:memento@localhost:5432/memento?sslmode=disable \
  go test ./internal/store/
```

Live network checks are opt-in: `MEMENTO_LIVE=1 go test ./internal/letterboxd/ -run TestLiveFetch -v`.

## Chart colors

The palette is computed, not chosen. Letterboxd's own accents sit at lightness
0.73–0.79 — outside the usable band for the dark chart surface — and fail
colorblind separation on the tritan axis. Data marks therefore use a validated
categorical set (lightness band, chroma floor, CVD separation, normal-vision
floor, and contrast all pass against `#14181c`), and the Letterboxd green is kept
for UI chrome where it carries no data meaning. Slot order is fixed: reordering
the hues puts orange next to yellow and fails separation.

## Scope and etiquette

Reads only public profile data, the same pages a logged-out visitor sees. The
shared cache is the main courtesy here: a film is fetched once and reused for
every user afterwards, which removes most repeat traffic the reference
implementation generates. Requests are paced, backed off, and capped.

Letterboxd has no public API, and scraping is against the spirit of their terms —
this is a personal project, not a service. If you want the same charts without
scraping, the site's own stats and a Pro subscription cover much of it.
