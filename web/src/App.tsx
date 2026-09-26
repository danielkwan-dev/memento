import { Suspense, lazy, useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, api } from './lib/api'
import type { Stats } from './lib/api'
import { useJobProgress } from './hooks/useJobProgress'
import { SyncProgress } from './components/SyncProgress'

// Recharts is the bulk of the bundle and nothing renders it until a sync
// finishes, so the dashboard is split out of the initial load. That keeps the
// landing page -- which is all a first-time visitor sees while the server wakes
// -- small and fast.
const Dashboard = lazy(() =>
  import('./components/Dashboard').then((m) => ({ default: m.Dashboard })),
)

type View =
  | { kind: 'idle' }
  | { kind: 'syncing'; username: string; jobId: string }
  | { kind: 'loading'; username: string }
  | { kind: 'ready'; username: string; stats: Stats }
  | {
      kind: 'failed'
      username: string
      message: string
      blocked?: boolean
      /** Which route failed, so the advice can match it. */
      via?: 'scrape' | 'upload'
    }

type ServerState = 'unknown' | 'waking' | 'ready'

export default function App() {
  const [view, setView] = useState<View>({ kind: 'idle' })
  const [input, setInput] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [file, setFile] = useState<File | null>(null)
  const [serverState, setServerState] = useState<ServerState>('unknown')
  const wakeStarted = useRef(false)
  // Which route started the current job, so a failure gives matching advice.
  const lastViaRef = useRef<'scrape' | 'upload'>('scrape')

  // Wake the backend as soon as the page loads.
  //
  // On free-tier hosting an idle machine is scaled to zero and takes seconds to
  // boot. Firing this during the time the user spends reading the page and typing
  // means the machine is usually warm by the time they submit, instead of them
  // waiting on a cold start afterwards. The API handler also pings the worker, so
  // one probe warms both processes.
  useEffect(() => {
    if (wakeStarted.current) return
    wakeStarted.current = true

    let cancelled = false
    setServerState('waking')

    const attempt = async (n: number): Promise<void> => {
      try {
        await api.wake()
        if (!cancelled) setServerState('ready')
      } catch {
        // A cold machine refuses connections at first; keep trying briefly.
        if (cancelled || n >= 5) {
          if (!cancelled) setServerState('unknown')
          return
        }
        await new Promise((r) => setTimeout(r, 1500 * (n + 1)))
        return attempt(n + 1)
      }
    }
    void attempt(0)

    return () => {
      cancelled = true
    }
  }, [])

  const jobId = view.kind === 'syncing' ? view.jobId : null
  const { progress, outcome, reconnecting } = useJobProgress(jobId)

  const loadStats = useCallback(async (username: string) => {
    setView({ kind: 'loading', username })
    try {
      const stats = await api.stats(username)
      setView({ kind: 'ready', username, stats })
    } catch (err) {
      setView({
        kind: 'failed',
        username,
        message:
          err instanceof ApiError ? err.message : 'Could not load the stats.',
      })
    }
  }, [])

  // React to the SSE stream finishing.
  useEffect(() => {
    if (view.kind !== 'syncing') return
    if (outcome.kind === 'complete') {
      void loadStats(view.username)
    } else if (outcome.kind === 'error') {
      setView({
        kind: 'failed',
        username: view.username,
        message: outcome.message,
        blocked: outcome.blocked,
        via: lastViaRef.current,
      })
    }
  }, [outcome, view, loadStats])

  // Scraping by username, and uploading an export, are two routes to the same
  // job. They are offered side by side rather than as modes: the upload is the
  // reliable one on a hosted deployment, where Letterboxd often will not serve
  // the paginated profile pages at all.
  const start = async (
    fn: () => Promise<{ job: { id: string } }>,
    via: 'scrape' | 'upload',
  ) => {
    const username = input.trim().toLowerCase()
    if (!username || submitting) return

    setSubmitting(true)
    try {
      const res = await fn()
      setView({ kind: 'syncing', username, jobId: res.job.id })
      lastViaRef.current = via
    } catch (err) {
      setView({
        kind: 'failed',
        username: input.trim().toLowerCase(),
        message:
          err instanceof ApiError
            ? err.message
            : 'Could not reach the server. Please try again.',
        via,
      })
    } finally {
      setSubmitting(false)
    }
  }

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    void start(() => api.sync(input.trim().toLowerCase()), 'scrape')
  }

  const handleUpload = () => {
    if (!file) return
    void start(() => api.import(input.trim().toLowerCase(), file), 'upload')
  }

  // The reference implementation changes this label once scraping has been
  // blocked, which is a good cue: before a failure the upload is an alternative,
  // after one it is the way forward.
  // Suggesting "upload an export instead" only makes sense when the SCRAPE was
  // blocked. Saying it after an upload already failed is nonsense.
  const blocked =
    view.kind === 'failed' && view.blocked === true && view.via !== 'upload'

  if (view.kind === 'ready') {
    return (
      <Suspense
        fallback={
          <p className="mt-24 text-center text-sm text-muted">
            Building your charts…
          </p>
        }
      >
        <Dashboard
          stats={view.stats}
          username={view.username}
          onReset={() => {
            setInput('')
            setView({ kind: 'idle' })
          }}
        />
      </Suspense>
    )
  }

  return (
    <main className="flex min-h-screen flex-col items-center justify-center px-4 py-12">
      <div className="w-full max-w-xl">
        <div className="mb-10 text-center">
          <h1 className="text-4xl font-semibold tracking-tight">
            mem<span className="text-accent">e</span>nto
          </h1>
          <p className="mx-auto mt-3 max-w-md text-sm leading-relaxed text-muted">
            Turn a public Letterboxd profile into charts: ratings, genres,
            directors, decades, obscurity and more.
          </p>
        </div>

        {(view.kind === 'idle' || view.kind === 'failed') && (
          <>
            <form onSubmit={handleSubmit} className="flex gap-2">
              <label htmlFor="username" className="sr-only">
                Letterboxd username
              </label>
              <input
                id="username"
                value={input}
                onChange={(e) => setInput(e.target.value)}
                placeholder="letterboxd username"
                autoComplete="off"
                autoCapitalize="off"
                spellCheck={false}
                className="min-w-0 flex-1 rounded-lg border border-border bg-surface px-4 py-3 text-sm text-text placeholder:text-muted/70 focus:border-accent focus:outline-none"
              />
              <button
                type="submit"
                disabled={!input.trim() || submitting}
                className="rounded-lg bg-accent px-5 py-3 text-sm font-semibold text-ink transition-opacity hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-40"
              >
                {submitting ? 'Starting…' : 'Start'}
              </button>
            </form>

            {serverState === 'waking' && (
              <p className="mt-3 text-center text-xs text-muted">
                <span className="mr-1.5 inline-block size-1.5 animate-pulse rounded-full bg-accent-3 align-middle" />
                Waking up the server…
              </p>
            )}

            {view.kind === 'failed' && (
              <div
                role="alert"
                className="mt-4 rounded-lg border border-accent-3/40 bg-accent-3/10 px-4 py-3 text-sm"
              >
                {blocked ? (
                  <>
                    <p className="font-medium text-accent-3">
                      Letterboxd wouldn't serve your film list to this server
                    </p>
                    <p className="mt-1 text-muted">
                      This usually happens on hosted deployments rather than when
                      running locally. Upload your data export below instead — it
                      skips the pages that get blocked.
                    </p>
                    <p className="mt-2 text-muted">
                      If the upload struggles too, running it locally avoids this
                      entirely.{' '}
                      <a
                        href="https://github.com/danielkwan-dev/memento#installation--usage"
                        target="_blank"
                        rel="noreferrer"
                        className="text-accent underline decoration-accent/40 hover:decoration-accent"
                      >
                        Setup instructions
                      </a>
                      .
                    </p>
                  </>
                ) : view.via === 'upload' && view.blocked ? (
                  <>
                    <p className="font-medium text-accent-3">
                      Your export loaded, but Letterboxd blocked the film details
                    </p>
                    <p className="mt-1 text-muted">
                      Genres, runtimes and cast come from Letterboxd itself, and
                      it is rate limiting this server. Trying again in a few
                      minutes often works, since anything already fetched is
                      cached and skipped.
                    </p>
                    <p className="mt-2 text-muted">
                      If it keeps failing, run it locally instead — a home
                      connection is treated far more leniently than a hosted one.{' '}
                      <a
                        href="https://github.com/danielkwan-dev/memento#installation--usage"
                        target="_blank"
                        rel="noreferrer"
                        className="text-accent underline decoration-accent/40 hover:decoration-accent"
                      >
                        Setup instructions
                      </a>{' '}
                      — it is one <code className="text-text">docker compose up</code>.
                    </p>
                  </>
                ) : (
                  <>
                    <p className="font-medium text-accent-3">
                      Couldn't analyse that profile
                    </p>
                    <p className="mt-1 text-muted">{view.message}</p>
                  </>
                )}
              </div>
            )}

            {/* The export upload is always available, not hidden behind a
                failure: on a hosted deployment it is the reliable route. */}
            <div className="mt-6 border-t border-border pt-5">
              <h2 className="text-sm font-medium text-text">
                {blocked
                  ? 'Upload your data export instead'
                  : 'Or upload your Letterboxd data export'}
              </h2>
              <p className="mt-1.5 text-xs leading-relaxed text-muted">
                On Letterboxd, go to{' '}
                <span className="font-medium text-text">
                  Settings → Data → Export Your Data
                </span>
                , then upload the ZIP here without unpacking it. This is faster
                than scraping.
              </p>

              <div className="mt-3 flex gap-2">
                <label
                  htmlFor="export"
                  className="min-w-0 flex-1 cursor-pointer truncate rounded-lg border border-dashed border-border bg-surface px-4 py-3 text-center text-sm text-muted transition-colors hover:border-muted hover:text-text"
                >
                  {file ? (
                    <span className="text-text">{file.name}</span>
                  ) : (
                    'Choose your export .zip'
                  )}
                </label>
                <input
                  id="export"
                  type="file"
                  accept=".zip,application/zip"
                  onChange={(e) => setFile(e.target.files?.[0] ?? null)}
                  className="sr-only"
                />
                <button
                  type="button"
                  onClick={handleUpload}
                  disabled={!file || !input.trim() || submitting}
                  className="shrink-0 rounded-lg border border-border px-4 py-3 text-sm font-medium text-text transition-colors hover:border-accent hover:text-accent disabled:cursor-not-allowed disabled:opacity-40"
                >
                  Upload
                </button>
              </div>
              {file && !input.trim() && (
                <p className="mt-2 text-xs text-accent-3">
                  Enter your username above as well, so the results can be saved
                  against it.
                </p>
              )}
            </div>

            <p className="mt-8 text-center text-xs leading-relaxed text-muted/80">
              Reads only public profile data. Film details are cached and shared
              across users, so popular films are never re-fetched.
            </p>
          </>
        )}

        {view.kind === 'syncing' && (
          <SyncProgress
            progress={progress}
            reconnecting={reconnecting}
            username={view.username}
          />
        )}

        {view.kind === 'loading' && (
          <p className="text-center text-sm text-muted">Building your charts…</p>
        )}
      </div>
    </main>
  )
}
