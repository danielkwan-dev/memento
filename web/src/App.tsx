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
  | { kind: 'failed'; username: string; message: string }

type ServerState = 'unknown' | 'waking' | 'ready'

export default function App() {
  const [view, setView] = useState<View>({ kind: 'idle' })
  const [input, setInput] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [serverState, setServerState] = useState<ServerState>('unknown')
  const wakeStarted = useRef(false)

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
      setView({ kind: 'failed', username: view.username, message: outcome.message })
    }
  }, [outcome, view, loadStats])

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const username = input.trim().toLowerCase()
    if (!username || submitting) return

    setSubmitting(true)
    try {
      const res = await api.sync(username)
      setView({ kind: 'syncing', username, jobId: res.job.id })
    } catch (err) {
      setView({
        kind: 'failed',
        username,
        message:
          err instanceof ApiError
            ? err.message
            : 'Could not reach the server. Please try again.',
      })
    } finally {
      setSubmitting(false)
    }
  }

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
                {submitting ? 'Starting…' : 'Analyse'}
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
                <p className="font-medium text-accent-3">Couldn't analyse that profile</p>
                <p className="mt-1 text-muted">{view.message}</p>
              </div>
            )}

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
