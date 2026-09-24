// Types mirror the Go structs in internal/api and internal/stats.

export type JobStatus =
  | 'queued'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'cancelled'

// 'waking' is a real backend phase, not a UI invention: on free-tier hosting the
// worker machine may be scaled to zero when the job is enqueued.
export type JobPhase =
  | 'waking'
  | 'resolve'
  | 'index'
  | 'hydrate'
  | 'persist'
  | 'done'

export interface Job {
  id: string
  username: string
  kind: 'scrape' | 'import'
  status: JobStatus
  phase: JobPhase
  films_total: number
  films_done: number
  cache_hits: number
  error?: string
  attempts: number
  created_at: string
  started_at?: string
  finished_at?: string
}

export interface JobProgress {
  job_id: string
  status: JobStatus
  phase: JobPhase
  films_total: number
  films_done: number
  cache_hits: number
  error?: string
}

export interface SyncResponse {
  job: Job
  already_in_flight?: boolean
  last_synced_at?: string
}

export interface Bucket {
  label: string
  count: number
  avg_rating?: number
}

export interface RatingBucket {
  rating: number
  count: number
}

export interface DayCount {
  date: string
  count: number
}

export interface Overview {
  films_logged: number
  diary_entries: number
  rated: number
  liked: number
  avg_rating?: number
  total_runtime_min: number
  distinct_years: number
  rewatches: number
}

export interface RareFilm {
  title: string
  watch_count: number
}

export interface Stats {
  overview: Overview
  ratings: RatingBucket[]
  activity: { daily: DayCount[]; monthly: DayCount[]; weekday: DayCount[] }
  genres: Bucket[]
  themes: Bucket[]
  decades: Bucket[]
  obscurity: { deciles: Bucket[]; rarest: RareFilm[] }
  runtime: Bucket[]
  people: { directors: Bucket[]; actors: Bucket[]; studios: Bucket[] }
  geography: { countries: Bucket[]; languages: Bucket[] }
}

export class ApiError extends Error {
  readonly status: number

  constructor(message: string, status: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...init?.headers },
  })
  if (!res.ok) {
    let message = `request failed (${res.status})`
    try {
      const body = (await res.json()) as { error?: string }
      if (body.error) message = body.error
    } catch {
      // Non-JSON error body; keep the status-based message.
    }
    throw new ApiError(message, res.status)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export const api = {
  /**
   * Wakes the backend. Called on page load so a scaled-to-zero machine starts
   * booting while the user is still reading the page, rather than after they
   * submit. The API handler also pings the worker, so this warms both.
   */
  wake: () => request<{ status: string; time: string }>('/healthz'),

  sync: (username: string) =>
    request<SyncResponse>('/api/v1/sync', {
      method: 'POST',
      body: JSON.stringify({ username }),
    }),

  job: (id: string) => request<Job>(`/api/v1/jobs/${id}`),

  stats: (username: string) =>
    request<Stats>(`/api/v1/users/${encodeURIComponent(username)}/stats`),

  deleteUser: (username: string) =>
    request<void>(`/api/v1/users/${encodeURIComponent(username)}`, {
      method: 'DELETE',
    }),
}

/** Human-readable label for each backend phase. */
export const phaseLabels: Record<JobPhase, string> = {
  waking: 'Waking up the server',
  resolve: 'Finding your profile',
  index: 'Reading your films and diary',
  hydrate: 'Fetching film details',
  persist: 'Saving results',
  done: 'Done',
}

/** Rough progress fraction across the whole job, for a single bar. */
export function overallProgress(p: JobProgress | null): number {
  if (!p) return 0
  switch (p.phase) {
    case 'waking':
      return 0.02
    case 'resolve':
      return 0.06
    case 'index':
      return 0.15
    case 'hydrate': {
      // Hydration dominates the runtime, so it owns most of the bar.
      if (p.films_total === 0) return 0.25
      const frac = Math.min(p.films_done / p.films_total, 1)
      return 0.2 + frac * 0.75
    }
    case 'persist':
      return 0.97
    case 'done':
      return 1
    default:
      return 0
  }
}
