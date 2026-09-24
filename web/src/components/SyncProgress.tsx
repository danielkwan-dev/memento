import { overallProgress, phaseLabels } from '../lib/api'
import type { JobPhase, JobProgress } from '../lib/api'

const phaseOrder: JobPhase[] = [
  'waking',
  'resolve',
  'index',
  'hydrate',
  'persist',
]

export function SyncProgress({
  progress,
  reconnecting,
  username,
}: {
  progress: JobProgress | null
  reconnecting: boolean
  username: string
}) {
  const phase = progress?.phase ?? 'waking'
  const pct = Math.round(overallProgress(progress) * 100)
  const currentIndex = phaseOrder.indexOf(phase)

  return (
    <div className="mx-auto w-full max-w-xl">
      <div className="rounded-xl border border-border bg-surface p-6">
        <div className="flex items-baseline justify-between">
          <h2 className="text-base font-semibold">
            Analysing <span className="text-accent">{username}</span>
          </h2>
          <span className="text-sm tabular-nums text-muted">{pct}%</span>
        </div>

        <div
          className="mt-4 h-2 w-full overflow-hidden rounded-full bg-surface-2"
          role="progressbar"
          aria-valuenow={pct}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-label="Sync progress"
        >
          <div
            className="h-full rounded-full bg-accent transition-[width] duration-500 ease-out"
            style={{ width: `${Math.max(pct, 2)}%` }}
          />
        </div>

        <p className="mt-4 text-sm text-text">
          {phaseLabels[phase]}
          {phase === 'hydrate' && progress && progress.films_total > 0 && (
            <span className="tabular-nums text-muted">
              {' '}
              — {progress.films_done} of {progress.films_total}
            </span>
          )}
        </p>

        {/* The cold start is explained rather than left looking stuck: on free
            hosting the first request of the day really does wait for a boot. */}
        {phase === 'waking' && (
          <p className="mt-2 text-xs leading-relaxed text-muted">
            The server sleeps when idle to keep hosting free, so the first run
            after a quiet spell takes a few extra seconds to start up.
          </p>
        )}

        {progress && progress.cache_hits > 0 && (
          <p className="mt-2 text-xs text-muted">
            <span className="tabular-nums text-accent-2">
              {progress.cache_hits}
            </span>{' '}
            {progress.cache_hits === 1 ? 'film was' : 'films were'} already
            cached, so they did not need fetching.
          </p>
        )}

        {reconnecting && (
          <p className="mt-2 text-xs text-accent-3">
            Connection dropped — reconnecting…
          </p>
        )}

        <ol className="mt-5 space-y-2">
          {phaseOrder.map((p, i) => {
            const state =
              i < currentIndex ? 'done' : i === currentIndex ? 'active' : 'todo'
            return (
              <li key={p} className="flex items-center gap-2.5 text-xs">
                <span
                  aria-hidden
                  className={
                    state === 'done'
                      ? 'size-1.5 rounded-full bg-accent'
                      : state === 'active'
                        ? 'size-1.5 animate-pulse rounded-full bg-accent-2'
                        : 'size-1.5 rounded-full bg-border'
                  }
                />
                <span
                  className={
                    state === 'todo'
                      ? 'text-muted/60'
                      : state === 'active'
                        ? 'text-text'
                        : 'text-muted'
                  }
                >
                  {phaseLabels[p]}
                </span>
              </li>
            )
          })}
        </ol>
      </div>
    </div>
  )
}
