import type { ReactNode } from 'react'

export function Card({
  title,
  subtitle,
  children,
  className = '',
}: {
  title?: string
  subtitle?: string
  children: ReactNode
  className?: string
}) {
  return (
    <section
      className={`rounded-xl border border-border bg-surface p-5 ${className}`}
    >
      {title && (
        <header className="mb-4">
          <h3 className="text-sm font-semibold tracking-wide text-text">
            {title}
          </h3>
          {subtitle && <p className="mt-1 text-xs text-muted">{subtitle}</p>}
        </header>
      )}
      {children}
    </section>
  )
}

/**
 * A single headline number. Values are rendered with tabular figures so a row of
 * tiles stays aligned as numbers change.
 */
export function StatTile({
  label,
  value,
  hint,
}: {
  label: string
  value: string
  hint?: string
}) {
  return (
    <div className="rounded-xl border border-border bg-surface px-4 py-3.5">
      <div className="text-xs font-medium uppercase tracking-wider text-muted">
        {label}
      </div>
      <div className="mt-1.5 text-2xl font-semibold tabular-nums text-text">
        {value}
      </div>
      {hint && <div className="mt-0.5 text-xs text-muted">{hint}</div>}
    </div>
  )
}

export function EmptyChart({ message }: { message: string }) {
  return (
    <div className="flex h-56 items-center justify-center rounded-lg border border-dashed border-border text-sm text-muted">
      {message}
    </div>
  )
}

/** Shared Recharts tooltip so every chart reads the same. */
export function tooltipStyle() {
  return {
    contentStyle: {
      backgroundColor: 'var(--color-surface-2)',
      border: '1px solid var(--color-border)',
      borderRadius: '8px',
      fontSize: '12px',
      color: 'var(--color-text)',
    },
    labelStyle: { color: 'var(--color-muted)', marginBottom: 4 },
    itemStyle: { color: 'var(--color-text)' },
  }
}

export function formatRuntime(minutes: number): string {
  if (!minutes) return '0h'
  const days = Math.floor(minutes / 1440)
  const hours = Math.floor((minutes % 1440) / 60)
  if (days > 0) return `${days}d ${hours}h`
  return `${hours}h ${minutes % 60}m`
}

export function formatCount(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`
  return String(n)
}
