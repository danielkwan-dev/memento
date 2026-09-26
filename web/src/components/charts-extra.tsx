import {
  CartesianGrid,
  Cell,
  Line,
  LineChart,
  Pie,
  PieChart,
  PolarAngleAxis,
  PolarGrid,
  PolarRadiusAxis,
  Radar,
  RadarChart,
  ReferenceLine,
  ResponsiveContainer,
  Scatter,
  ScatterChart,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import type { Bucket, MonthAvg, ScatterPoint } from '../lib/api'
import { EmptyChart, tooltipStyle } from './primitives'
import { AXIS, GRID, LBX, LBX_MUTED } from '../lib/viz'

const AXIS_PROPS = {
  stroke: AXIS,
  tick: { fill: AXIS, fontSize: 12 },
  tickLine: false,
} as const

function numFormatter(fn: (n: number) => string) {
  return (value: unknown): [string, string] => {
    const n = typeof value === 'number' ? value : Number(value ?? 0)
    return [fn(Number.isFinite(n) ? n : 0), '']
  }
}

/**
 * Liked vs not-liked, as a donut.
 *
 * Two slices only — a pie earns its place at a part-to-whole comparison this
 * simple, where a bar chart would be two lonely columns. The headline percentage
 * sits in the hole rather than in a legend.
 */
export function LikedPie({
  liked,
  notLiked,
}: {
  liked: number
  notLiked: number
}) {
  const total = liked + notLiked
  if (total === 0) return <EmptyChart message="No films logged" />

  const data = [
    { name: 'Liked', value: liked, fill: LBX.orange },
    { name: 'Not liked', value: notLiked, fill: LBX_MUTED },
  ]
  const pct = Math.round((liked / total) * 100)

  return (
    <div className="relative">
      <ResponsiveContainer width="100%" height={260}>
        <PieChart>
          <Pie
            data={data}
            dataKey="value"
            nameKey="name"
            innerRadius="58%"
            outerRadius="82%"
            paddingAngle={2}
            strokeWidth={0}
          >
            {data.map((d) => (
              <Cell key={d.name} fill={d.fill} />
            ))}
          </Pie>
          <Tooltip
            {...tooltipStyle()}
            formatter={(value: unknown, name: unknown) => {
              const n = typeof value === 'number' ? value : Number(value ?? 0)
              return [
                `${n} films (${Math.round((n / total) * 100)}%)`,
                String(name),
              ]
            }}
          />
        </PieChart>
      </ResponsiveContainer>
      <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
        <span className="text-3xl font-semibold tabular-nums text-text">
          {pct}%
        </span>
        <span className="text-xs text-muted">liked</span>
      </div>
    </div>
  )
}

/**
 * The user's rating against Letterboxd's average, per film.
 *
 * The most informative chart here: the dashed diagonal is agreement with the
 * crowd, so points above it are films you liked more than everyone else and
 * points below are the ones you liked less.
 */
export function RatingScatter({ data }: { data: ScatterPoint[] }) {
  if (!data || data.length < 3) {
    return <EmptyChart message="Not enough rated films" />
  }
  return (
    <ResponsiveContainer width="100%" height={300}>
      <ScatterChart margin={{ top: 10, right: 16, left: -10, bottom: 12 }}>
        <CartesianGrid stroke={GRID} />
        <XAxis
          type="number"
          dataKey="avg_rating"
          domain={[0, 5]}
          ticks={[0, 1, 2, 3, 4, 5]}
          {...AXIS_PROPS}
          label={{
            value: 'Letterboxd average',
            position: 'insideBottom',
            offset: -6,
            fill: AXIS,
            fontSize: 11,
          }}
        />
        <YAxis
          type="number"
          dataKey="rating"
          domain={[0, 5]}
          ticks={[0, 1, 2, 3, 4, 5]}
          {...AXIS_PROPS}
          label={{
            value: 'Your rating',
            angle: -90,
            position: 'insideLeft',
            offset: 18,
            fill: AXIS,
            fontSize: 11,
          }}
        />
        {/* On this line you rated a film exactly as the crowd did. */}
        <ReferenceLine
          segment={[
            { x: 0, y: 0 },
            { x: 5, y: 5 },
          ]}
          stroke={AXIS}
          strokeDasharray="4 4"
          strokeOpacity={0.5}
        />
        <Tooltip
          {...tooltipStyle()}
          cursor={{ strokeDasharray: '3 3', stroke: AXIS }}
          content={({ active, payload }) => {
            if (!active || !payload?.length) return null
            const p = payload[0].payload as ScatterPoint
            return (
              <div className="rounded-lg border border-border bg-surface-2 px-3 py-2 text-xs">
                <div className="font-medium text-text">
                  {p.title}
                  {p.year ? ` (${p.year})` : ''}
                </div>
                <div className="mt-1 text-muted">
                  You:{' '}
                  <span className="tabular-nums text-text">{p.rating}★</span>
                  {' · '}
                  Average:{' '}
                  <span className="tabular-nums text-text">
                    {p.avg_rating.toFixed(2)}★
                  </span>
                </div>
              </div>
            )
          }}
        />
        <Scatter data={data} fill={LBX.blue} fillOpacity={0.75} />
      </ScatterChart>
    </ResponsiveContainer>
  )
}

/**
 * A radar over the top categories.
 *
 * Reads as a shape rather than a ranking, which suits "how broad is my taste"
 * better than bars: a spiky outline means a few dominant categories, an even one
 * means variety.
 */
export function CategoryRadar({
  data,
  limit = 8,
  color = LBX.green,
}: {
  data: Bucket[]
  limit?: number
  color?: string
}) {
  const rows = (data ?? []).slice(0, limit)
  if (rows.length < 3) return <EmptyChart message="Not enough categories" />

  return (
    <ResponsiveContainer width="100%" height={300}>
      <RadarChart data={rows} outerRadius="70%">
        <PolarGrid stroke={GRID} />
        <PolarAngleAxis dataKey="label" tick={{ fill: AXIS, fontSize: 11 }} />
        <PolarRadiusAxis
          tick={{ fill: AXIS, fontSize: 10 }}
          stroke={GRID}
          allowDecimals={false}
        />
        <Tooltip
          {...tooltipStyle()}
          formatter={numFormatter((n) => `${n} films`)}
        />
        <Radar
          dataKey="count"
          stroke={color}
          fill={color}
          fillOpacity={0.3}
          strokeWidth={2}
        />
      </RadarChart>
    </ResponsiveContainer>
  )
}

/** Average rating per month: shows whether your taste is drifting over time. */
export function RatingTrendLine({ data }: { data: MonthAvg[] }) {
  if (!data || data.length < 2) {
    return <EmptyChart message="Not enough dated ratings" />
  }
  return (
    <ResponsiveContainer width="100%" height={240}>
      <LineChart data={data} margin={{ top: 8, right: 12, left: -16, bottom: 0 }}>
        <CartesianGrid stroke={GRID} vertical={false} />
        <XAxis dataKey="month" {...AXIS_PROPS} minTickGap={28} />
        <YAxis domain={[0, 5]} ticks={[0, 1, 2, 3, 4, 5]} {...AXIS_PROPS} />
        <Tooltip
          {...tooltipStyle()}
          formatter={(value: unknown, _n: unknown, item: unknown) => {
            const n = typeof value === 'number' ? value : Number(value ?? 0)
            const c = (item as { payload?: MonthAvg } | undefined)?.payload?.count
            return [`${n.toFixed(2)}★ over ${c ?? 0} films`, '']
          }}
          cursor={{ stroke: AXIS, strokeDasharray: '3 3' }}
        />
        <Line
          type="monotone"
          dataKey="avg"
          stroke={LBX.orange}
          strokeWidth={2}
          dot={{ r: 3, fill: LBX.orange, strokeWidth: 0 }}
          activeDot={{ r: 5, strokeWidth: 2, stroke: 'var(--color-surface)' }}
        />
      </LineChart>
    </ResponsiveContainer>
  )
}
