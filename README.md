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

**Detail-page concurrency is measured, not assumed.** The reference project
reasons that film pages tolerate high parallelism because they sit in
Cloudflare's edge cache while user pages do not. Measured against the live site,
that holds: 12 film pages at concurrency 16 complete in ~281ms with zero blocks
and zero retries, and 4/8/16 are indistinguishable. The user-page limit is the one
that actually binds.

**Two concurrency limits, plus a rate limit.** A user's grid and diary pages are
not in Cloudflare's edge cache, so they run at 3 concurrent; film detail pages are
edge-cached and run at 16. Same reasoning as the reference project's split thread
pools, expressed as two `semaphore.Weighted` instances.

Concurrency alone turned out to be insufficient: a semaphore bounds requests *in
flight*, not requests *per second*, and Cloudflare limits the latter. A 36-page
profile at 3-concurrent is still a burst. Index requests are therefore also paced
to a minimum interval — measured, not guessed: unpaced and at 400ms the index
phase gets blocked partway through; ~1.5s apart it completes with zero retries.

**Letterboxd's paginator over-reports.** On a 2520-film profile it links page 36,
then serves 403 for page 36 *and* page 37 (past the end), while pages 1–35 return
in under 300ms. Because `errgroup` cancels siblings on the first error, one bogus
page was discarding 35 good ones. A failure on the *last advertised* page now
means "no more data"; a failure on any earlier page stays fatal, since silently
dropping page 3 of 10 would store an incomplete profile as if it were complete.

**TLS fingerprint spoofing.** Letterboxd rejects clients whose TLS handshake isn't
a real browser's. Go's `net/http` is trivially identifiable, so the fetcher uses
[`bogdanfinn/tls-client`](https://github.com/bogdanfinn/tls-client) (uTLS +
`fhttp`) to replay real Chrome/Firefox/Safari ClientHellos and HTTP/2 frame
ordering. The difference is measurable: `/csi/film/{slug}/stats/` returns **403**
to curl — even with a spoofed `User-Agent` — and **200** to this client.

**Blocks are rate limiting, not fingerprint detection — and the limit is per-IP
and cumulative.** This one took real measurement to pin down. Mid-scrape, every
request can start failing while plain `curl` keeps getting 200 on the same URLs,
which looks exactly like the fingerprint being rejected. It is not: after a
cooldown, all six TLS profiles return 200 on the very URLs that had just failed 24
times in a row. What actually happens is that a long scrape spends a per-IP budget
and earns a temporary block, during which *nothing* gets through regardless of
concurrency or fingerprint.

Two consequences. Rotating fingerprints eagerly makes it worse, because it retries
a hot path under a fresh identity, so rotation needs a long block streak and
blocked requests simply back off harder. And pacing spends the budget more slowly
but cannot clear an active block — only waiting does. The shared film cache is the
real mitigation, since it removes most requests entirely.

**Failure is proportionate — and fast.** One deleted film must not abandon a
2000-film scrape, so per-film failures are counted, not propagated. If more than
20% fail, that is a broken parser or a blocking upstream rather than missing films,
and the job fails loudly instead of storing a gutted profile.

That design had a structural flaw worth recording, found by running against a live
profile while rate limited: because per-film failures return `nil`, the ratio check
only sees totals *after* every film has burned 6 attempts backing off to 60s. A
242-film job took the full 25-minute timeout to report a failure it could have
called after the first handful. A circuit breaker now aborts hydrate once the
running rate is hopeless.

Calibrating it needed a second pass. Failures cluster — films are fetched in
roughly slug order, so an early run of deleted films makes the in-flight ratio look
far worse than the final one (a test with a true 10% rate saw 6 of its first 25
fail). The breaker therefore needs 40 samples and 2x slack over the threshold: it
exists to escape a hopeless run, not to enforce the limit, which the final check
still does.

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
