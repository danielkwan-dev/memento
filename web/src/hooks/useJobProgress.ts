import { useEffect, useRef, useState } from 'react'
import type { JobProgress } from '../lib/api'

type Outcome =
  | { kind: 'pending' }
  | { kind: 'complete' }
  | { kind: 'error'; message: string }

export interface JobProgressState {
  progress: JobProgress | null
  outcome: Outcome
  /** True while the stream is disconnected and retrying. */
  reconnecting: boolean
}

/**
 * Subscribes to a job's SSE stream.
 *
 * EventSource reconnects on its own after a network drop, but it treats a
 * non-2xx response as a fatal error, which is exactly what a cold-starting
 * backend returns. So reconnection is handled explicitly with a capped backoff
 * rather than relying on the browser's default.
 */
export function useJobProgress(jobId: string | null): JobProgressState {
  const [progress, setProgress] = useState<JobProgress | null>(null)
  const [outcome, setOutcome] = useState<Outcome>({ kind: 'pending' })
  const [reconnecting, setReconnecting] = useState(false)

  // Kept in refs so the reconnect timer never resurrects a finished stream.
  const attemptsRef = useRef(0)
  const doneRef = useRef(false)

  useEffect(() => {
    if (!jobId) return

    doneRef.current = false
    attemptsRef.current = 0
    setProgress(null)
    setOutcome({ kind: 'pending' })
    setReconnecting(false)

    let source: EventSource | null = null
    let retryTimer: number | undefined

    const connect = () => {
      if (doneRef.current) return

      source = new EventSource(`/api/v1/jobs/${jobId}/events`)

      source.addEventListener('open', () => {
        attemptsRef.current = 0
        setReconnecting(false)
      })

      source.addEventListener('progress', (event) => {
        try {
          setProgress(JSON.parse((event as MessageEvent).data) as JobProgress)
          setReconnecting(false)
        } catch {
          // Ignore a malformed frame rather than tearing down the stream.
        }
      })

      source.addEventListener('complete', (event) => {
        try {
          setProgress(JSON.parse((event as MessageEvent).data) as JobProgress)
        } catch {
          /* keep the last good progress */
        }
        doneRef.current = true
        setOutcome({ kind: 'complete' })
        source?.close()
      })

      source.addEventListener('error', (event) => {
        // The server's own terminal 'error' event carries a payload; a transport
        // failure does not. Only the former is fatal.
        const data = (event as MessageEvent).data
        if (data) {
          let message = 'The sync failed.'
          try {
            const parsed = JSON.parse(data) as JobProgress & { error?: string }
            if (parsed.error) message = parsed.error
            setProgress(parsed)
          } catch {
            /* fall back to the generic message */
          }
          doneRef.current = true
          setOutcome({ kind: 'error', message })
          source?.close()
          return
        }

        // Transport failure: close this EventSource and retry with backoff.
        source?.close()
        if (doneRef.current) return

        attemptsRef.current += 1
        if (attemptsRef.current > 12) {
          setOutcome({
            kind: 'error',
            message: 'Lost connection to the server. Please try again.',
          })
          return
        }
        setReconnecting(true)
        const delay = Math.min(1000 * 2 ** (attemptsRef.current - 1), 8000)
        retryTimer = window.setTimeout(connect, delay)
      })
    }

    connect()

    return () => {
      doneRef.current = true
      if (retryTimer) window.clearTimeout(retryTimer)
      source?.close()
    }
  }, [jobId])

  return { progress, outcome, reconnecting }
}
