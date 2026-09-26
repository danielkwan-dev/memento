import {
  Bar,
  BarChart,
  CartesianGrid,
  Cell,
  Line,
  LineChart,
  PolarAngleAxis,
  PolarGrid,
  PolarRadiusAxis,
  Radar,
  RadarChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import type { Bucket, DayCount, RatingBucket } from '../lib/api'
import { EmptyChart, shortLabel, tooltipStyle } from './primitives'
import { AXIS, GRID, LBX, heatStep } from '../lib/viz'

/**
 * Recharts types tooltip values as `ValueType | undefined`, so these adapters
 * narrow to a number once rather than casting at every call site.
 */
function numFormatter(fn: (n: number) => string) {
  return (value: unknown): [string, string] => {
    const n = typeof value === 'number' ? value : Number(value ?? 0)
    return [fn(Number.isFinite(n) ? n : 0), '']
  }
}

function labelFormatter(fn: (label: string) => string) {
  return (label: unknown): string => fn(String(label ?? ''))
}

const AXIS_PROPS = {
  stroke: AXIS,
  tick: { fill: AXIS, fontSize: 12 },
  tickLine: false,
} as const

/** Rating histogram. One series, so no legend: the title names it. */
export function RatingHistogram({ data }: { data: RatingBucket[] }) {
  if (!data?.some((d) => d.count > 0)) {
    return <EmptyChart message="No ratings yet" />
  }
  return (
    <ResponsiveContainer width="100%" height={240}>
      <BarChart data={data} margin={{ top: 8, right: 8, left: -16, bottom: 0 }}>
        <CartesianGrid stroke={GRID} vertical={false} />
        <XAxis
          dataKey="rating"
          {...AXIS_PROPS}
          tickFormatter={(v: number) => '★'.repeat(Math.floor(v)) + (v % 1 ? '½' : '')}
          interval={0}
        />
        <YAxis {...AXIS_PROPS} allowDecimals={false} />
        <Tooltip
          {...tooltipStyle()}
          formatter={numFormatter((n) => `${n} films`)}
          labelFormatter={labelFormatter((l) => `${l} stars`)}
        />
        {/* 4px rounded data-end, anchored to the baseline. */}
        <Bar dataKey="count" fill={LBX.green} radius={[4, 4, 0, 0]} maxBarSize={44} />
      </BarChart>
    </ResponsiveContainer>
  )
}

/** Horizontal bars for ranked categories: labels read left-to-right. */
export function RankedBars({
  data,
  limit = 12,
  color = LBX.blue,
}: {
  data: Bucket[]
  limit?: number
  color?: string
}) {
  const rows = (data ?? []).slice(0, limit)
  if (rows.length === 0) return <EmptyChart message="Not enough data" />

  // Letterboxd's theme names are whole sentences. Shortening them at a word
  // boundary keeps the identifying part readable AND leaves the bars room to be
  // compared, which a 240px gutter did not. The full name stays in the tooltip.
  const shown = rows.map((r) => ({ ...r, short: shortLabel(r.label) }))
  const longest = Math.max(...shown.map((r) => r.short.length))
  const labelWidth = Math.min(Math.max(96, longest * 6.1), 175)

  return (
    <ResponsiveContainer width="100%" height={Math.max(220, rows.length * 28)}>
      <BarChart
        data={shown}
        layout="vertical"
        margin={{ top: 4, right: 16, left: 8, bottom: 4 }}
        barCategoryGap={2 /* 2px surface gap between adjacent fills */}
      >
        <CartesianGrid stroke={GRID} horizontal={false} />
        <XAxis type="number" {...AXIS_PROPS} allowDecimals={false} />
        <YAxis
          type="category"
          dataKey="short"
          {...AXIS_PROPS}
          tick={{ fill: AXIS, fontSize: 11 }}
          width={labelWidth}
          interval={0}
        />
        <Tooltip
          {...tooltipStyle()}
          labelFormatter={(_l: unknown, payload: readonly { payload?: Bucket }[]) =>
            payload?.[0]?.payload?.label ?? ''
          }
          formatter={(value: unknown, _name: unknown, item: unknown) => {
            const n = typeof value === 'number' ? value : Number(value ?? 0)
            const avg = (item as { payload?: Bucket } | undefined)?.payload
              ?.avg_rating
            return [
              avg ? `${n} films · ${avg.toFixed(2)}★ avg` : `${n} films`,
              '',
            ] as [string, string]
          }}
        />
        <Bar dataKey="count" fill={color} radius={[0, 4, 4, 0]} />
      </BarChart>
    </ResponsiveContainer>
  )
}

/** Radar for decades: a closed cyclical-ish shape reads well for era spread. */
export function DecadeRadar({ data }: { data: Bucket[] }) {
  if (!data || data.length < 3) return <EmptyChart message="Not enough decades" />
  return (
    <ResponsiveContainer width="100%" height={280}>
      <RadarChart data={data} outerRadius="72%">
        <PolarGrid stroke={GRID} />
        <PolarAngleAxis dataKey="label" tick={{ fill: AXIS, fontSize: 12 }} />
        <PolarRadiusAxis
          tick={{ fill: AXIS, fontSize: 10 }}
          stroke={GRID}
          allowDecimals={false}
        />
        <Tooltip {...tooltipStyle()} formatter={numFormatter((n) => `${n} films`)} />
        <Radar
          dataKey="count"
          stroke={LBX.green}
          fill={LBX.green}
          fillOpacity={0.35}
          strokeWidth={2}
        />
      </RadarChart>
    </ResponsiveContainer>
  )
}

/** Films watched per month: 2px line, crosshair tooltip. */
export function ActivityLine({ data }: { data: DayCount[] }) {
  if (!data || data.length < 2) return <EmptyChart message="Not enough diary history" />
  return (
    <ResponsiveContainer width="100%" height={240}>
      <LineChart data={data} margin={{ top: 8, right: 12, left: -16, bottom: 0 }}>
        <CartesianGrid stroke={GRID} vertical={false} />
        <XAxis dataKey="date" {...AXIS_PROPS} minTickGap={32} />
        <YAxis {...AXIS_PROPS} allowDecimals={false} />
        <Tooltip
          {...tooltipStyle()}
          formatter={numFormatter((n) => `${n} films`)}
          cursor={{ stroke: AXIS, strokeDasharray: '3 3' }}
        />
        <Line
          type="monotone"
          dataKey="count"
          stroke={LBX.blue}
          strokeWidth={2}
          dot={false}
          activeDot={{ r: 4, strokeWidth: 2, stroke: 'var(--color-surface)' }}
        />
      </LineChart>
    </ResponsiveContainer>
  )
}

export function WeekdayBars({ data }: { data: DayCount[] }) {
  if (!data || data.length === 0) return <EmptyChart message="No diary entries" />
  return (
    <ResponsiveContainer width="100%" height={200}>
      <BarChart data={data} margin={{ top: 8, right: 8, left: -16, bottom: 0 }}>
        <CartesianGrid stroke={GRID} vertical={false} />
        <XAxis
          dataKey="date"
          {...AXIS_PROPS}
          tickFormatter={(v: string) => v.slice(0, 3)}
          interval={0}
        />
        <YAxis {...AXIS_PROPS} allowDecimals={false} />
        <Tooltip {...tooltipStyle()} formatter={numFormatter((n) => `${n} films`)} />
        <Bar dataKey="count" fill={LBX.orange} radius={[4, 4, 0, 0]} maxBarSize={40} />
      </BarChart>
    </ResponsiveContainer>
  )
}

/**
 * Calendar heatmap of the last year, GitHub-style.
 *
 * Built as plain CSS grid rather than a chart library: the mark is a small square
 * per day, magnitude is a single-hue sequential step, and every cell carries a
 * title for hover. A charting library adds nothing here.
 */
export function CalendarHeatmap({ data }: { data: DayCount[] }) {
  if (!data || data.length === 0) {
    return <EmptyChart message="No dated diary entries" />
  }

  const counts = new Map(data.map((d) => [d.date, d.count]))
  const max = Math.max(...data.map((d) => d.count), 1)

  // 53 weeks back from the most recent Sunday, so columns are whole weeks.
  const end = new Date()
  end.setUTCHours(0, 0, 0, 0)
  end.setUTCDate(end.getUTCDate() + (6 - end.getUTCDay()))
  const start = new Date(end)
  start.setUTCDate(start.getUTCDate() - 53 * 7 + 1)

  const weeks: { date: string; count: number }[][] = []
  const cursor = new Date(start)
  while (cursor <= end) {
    const week: { date: string; count: number }[] = []
    for (let d = 0; d < 7; d++) {
      const iso = cursor.toISOString().slice(0, 10)
      week.push({ date: iso, count: counts.get(iso) ?? 0 })
      cursor.setUTCDate(cursor.getUTCDate() + 1)
    }
    weeks.push(week)
  }

  return (
    <div>
      {/* Wide content scrolls in its own container; the page never scrolls
          horizontally. */}
      <div className="overflow-x-auto pb-2">
        <div className="flex gap-[3px]">
          {weeks.map((week, i) => (
            <div key={i} className="flex flex-col gap-[3px]">
              {week.map((day) => (
                <div
                  key={day.date}
                  title={`${day.date}: ${day.count} ${day.count === 1 ? 'film' : 'films'}`}
                  className="size-[11px] rounded-[2px]"
                  style={{ backgroundColor: heatStep(day.count, max) }}
                />
              ))}
            </div>
          ))}
        </div>
      </div>
      <div className="mt-3 flex items-center gap-2 text-xs text-muted">
        <span>Less</span>
        {[0, 1, 2, 3, 4, 5, 6].map((step) => (
          <div
            key={step}
            className="size-[11px] rounded-[2px]"
            style={{ backgroundColor: heatStep(step, 6) }}
          />
        ))}
        <span>More</span>
        <span className="ml-2">· peak {max}/day</span>
      </div>
    </div>
  )
}

/** Obscurity deciles: how mainstream the user's taste is. */
export function ObscurityBars({ data }: { data: Bucket[] }) {
  if (!data?.some((d) => d.count > 0)) {
    return <EmptyChart message="No popularity data yet" />
  }
  return (
    <ResponsiveContainer width="100%" height={220}>
      <BarChart data={data} margin={{ top: 8, right: 8, left: -16, bottom: 0 }}>
        <CartesianGrid stroke={GRID} vertical={false} />
        <XAxis dataKey="label" {...AXIS_PROPS} interval={0} angle={-35} height={48} textAnchor="end" />
        <YAxis {...AXIS_PROPS} allowDecimals={false} />
        <Tooltip
          {...tooltipStyle()}
          formatter={numFormatter((n) => `${n} films`)}
          labelFormatter={labelFormatter((l) => `${l} most obscure`)}
        />
        <Bar dataKey="count" radius={[4, 4, 0, 0]} maxBarSize={44}>
          {/* A sequential step per bar: the axis is itself an ordered magnitude. */}
          {data.map((d, i) => (
            <Cell key={d.label} fill={heatStep(i + 1, data.length)} />
          ))}
        </Bar>
      </BarChart>
    </ResponsiveContainer>
  )
}
