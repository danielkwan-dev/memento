import { useState } from 'react'
import { api } from '../lib/api'
import type { Stats } from '../lib/api'
import {
  Card,
  StatTile,
  formatCount,
  formatRuntime,
} from './primitives'
import {
  ActivityLine,
  CalendarHeatmap,
  DecadeRadar,
  ObscurityBars,
  RankedBars,
  RatingHistogram,
  WeekdayBars,
} from './charts'
import {
  CategoryRadar,
  LikedPie,
  RatingScatter,
  RatingTrendLine,
} from './charts-extra'
import { LBX } from '../lib/viz'

type Tab = 'overview' | 'taste' | 'time' | 'people' | 'world'

const tabs: { id: Tab; label: string }[] = [
  { id: 'overview', label: 'Overview' },
  { id: 'taste', label: 'Taste' },
  { id: 'time', label: 'Time' },
  { id: 'people', label: 'People' },
  { id: 'world', label: 'World' },
]

export function Dashboard({
  stats,
  username,
  onReset,
}: {
  stats: Stats
  username: string
  onReset: () => void
}) {
  const [tab, setTab] = useState<Tab>('overview')
  const [deleting, setDeleting] = useState(false)
  const o = stats.overview

  // The backend removes this user's entries and cached stats while leaving the
  // global film cache alone -- that table holds only public facts about films.
  const handleDelete = async () => {
    if (deleting) return
    if (
      !window.confirm(
        `Delete stored data for ${username}? Your ratings and diary entries are removed. ` +
          `Cached film details stay, since they are public facts about films, not about you.`,
      )
    ) {
      return
    }
    setDeleting(true)
    try {
      await api.deleteUser(username)
      onReset()
    } catch {
      setDeleting(false)
    }
  }

  return (
    <div className="mx-auto w-full max-w-6xl px-4 pb-16">
      <header className="flex flex-wrap items-end justify-between gap-4 py-8">
        <div>
          <h1 className="text-2xl font-semibold">
            <span className="text-accent">{username}</span>
            <span className="text-muted"> · film stats</span>
          </h1>
          <p className="mt-1 text-sm text-muted">
            {o.films_logged.toLocaleString()} films logged
            {o.diary_entries > 0 &&
              ` · ${o.diary_entries.toLocaleString()} diary entries`}
          </p>
        </div>
        <div className="flex gap-2">
          <button
            onClick={handleDelete}
            disabled={deleting}
            className="rounded-lg border border-border px-3.5 py-2 text-sm text-muted transition-colors hover:border-accent-3 hover:text-accent-3 disabled:opacity-50"
          >
            {deleting ? 'Deleting…' : 'Delete my data'}
          </button>
          <button
            onClick={onReset}
            className="rounded-lg border border-border px-3.5 py-2 text-sm text-muted transition-colors hover:border-muted hover:text-text"
          >
            Analyse someone else
          </button>
        </div>
      </header>

      {/* Filters/navigation in one row above the charts. */}
      <nav className="mb-6 flex gap-1 overflow-x-auto border-b border-border">
        {tabs.map((t) => (
          <button
            key={t.id}
            onClick={() => setTab(t.id)}
            aria-current={tab === t.id ? 'page' : undefined}
            className={`-mb-px whitespace-nowrap border-b-2 px-4 py-2.5 text-sm transition-colors ${
              tab === t.id
                ? 'border-accent text-text'
                : 'border-transparent text-muted hover:text-text'
            }`}
          >
            {t.label}
          </button>
        ))}
      </nav>

      {tab === 'overview' && (
        <div className="space-y-5">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
            <StatTile label="Films" value={o.films_logged.toLocaleString()} />
            <StatTile
              label="Average rating"
              value={o.avg_rating ? `${o.avg_rating.toFixed(2)}★` : '—'}
              hint={`${o.rated.toLocaleString()} rated`}
            />
            <StatTile
              label="Watch time"
              value={formatRuntime(o.total_runtime_min)}
            />
            <StatTile label="Liked" value={o.liked.toLocaleString()} />
            <StatTile label="Diary entries" value={o.diary_entries.toLocaleString()} />
            <StatTile label="Rewatches" value={o.rewatches.toLocaleString()} />
            <StatTile label="Release years" value={String(o.distinct_years)} />
            <StatTile
              label="Top genre"
              value={stats.genres?.[0]?.label ?? '—'}
              hint={
                stats.genres?.[0] ? `${stats.genres[0].count} films` : undefined
              }
            />
          </div>

          <div className="grid gap-5 lg:grid-cols-3">
            <Card
              title="Rating distribution"
              subtitle="How you spread your stars"
              className="lg:col-span-2"
            >
              <RatingHistogram data={stats.ratings.histogram} />
            </Card>
            <Card title="Liked films" subtitle="What earned a heart">
              <LikedPie
                liked={stats.ratings.liked.liked}
                notLiked={stats.ratings.liked.not_liked}
              />
            </Card>
          </div>

          <Card
            title="You vs everyone else"
            subtitle="Above the line means you rated it higher than the Letterboxd average"
          >
            <RatingScatter data={stats.ratings.scatter} />
          </Card>

          <Card
            title="Viewing activity"
            subtitle="Diary entries over the last year"
          >
            <CalendarHeatmap data={stats.activity?.daily ?? []} />
          </Card>
        </div>
      )}

      {tab === 'taste' && (
        <div className="grid gap-5 lg:grid-cols-2">
          <Card title="Genres" subtitle="Most-watched, with your average rating on hover">
            <RankedBars data={stats.genres} />
          </Card>
          <Card title="Genre spread" subtitle="A spiky shape means narrow taste">
            <CategoryRadar data={stats.genres} />
          </Card>
          <Card title="Themes" subtitle="Letterboxd's thematic tags">
            <RankedBars data={stats.themes} color={LBX.orange} />
          </Card>
          <Card title="Theme spread" subtitle="The moods you gravitate to">
            <CategoryRadar data={stats.themes} color={LBX.orange} />
          </Card>
          <Card title="Runtime" subtitle="Film length in 30-minute bands">
            <RankedBars data={stats.runtime} color={LBX.green} limit={10} />
          </Card>
          <Card
            title="Obscurity"
            subtitle="Your films ranked against every film in the database"
          >
            <ObscurityBars data={stats.obscurity?.deciles ?? []} />
            {stats.obscurity?.rarest?.length > 0 && (
              <div className="mt-4 border-t border-border pt-3">
                <h4 className="mb-2 text-xs font-medium uppercase tracking-wider text-muted">
                  Your most obscure finds
                </h4>
                <ul className="space-y-1">
                  {stats.obscurity.rarest.slice(0, 5).map((f) => (
                    <li
                      key={f.title}
                      className="flex justify-between gap-4 text-sm"
                    >
                      <span className="truncate text-text">{f.title}</span>
                      <span className="shrink-0 tabular-nums text-muted">
                        {formatCount(f.watch_count)} watches
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            )}
          </Card>
        </div>
      )}

      {tab === 'time' && (
        <div className="space-y-5">
          <Card title="Films per month" subtitle="Your diary over time">
            <ActivityLine data={stats.activity?.monthly ?? []} />
          </Card>
          <Card
            title="Average rating over time"
            subtitle="Whether you have grown more generous or more picky"
          >
            <RatingTrendLine data={stats.activity?.rating_trend ?? []} />
          </Card>
          <div className="grid gap-5 lg:grid-cols-2">
            <Card title="Decades" subtitle="Era spread of what you watch">
              <DecadeRadar data={stats.decades} />
            </Card>
            <Card title="Day of the week" subtitle="When you log films">
              <WeekdayBars data={stats.activity?.weekday ?? []} />
            </Card>
          </div>
        </div>
      )}

      {tab === 'people' && (
        <div className="grid gap-5 lg:grid-cols-2">
          <Card title="Directors" subtitle="Most-watched">
            <RankedBars data={stats.people?.directors ?? []} />
          </Card>
          <Card title="Actors" subtitle="Top billing only, so ensembles don't dominate">
            <RankedBars data={stats.people?.actors ?? []} color={LBX.orange} />
          </Card>
          <Card title="Directors at a glance" subtitle="Your most-watched, as a shape">
            <CategoryRadar data={stats.people?.directors ?? []} color={LBX.blue} />
          </Card>
          <Card title="Studios" subtitle="Production companies">
            <RankedBars data={stats.people?.studios ?? []} color={LBX.green} limit={15} />
          </Card>
        </div>
      )}

      {tab === 'world' && (
        <div className="grid gap-5 lg:grid-cols-2">
          <Card title="Countries" subtitle="Where your films were made">
            <RankedBars data={stats.geography?.countries ?? []} limit={15} />
          </Card>
          <Card title="Languages" subtitle="Spoken languages">
            <RankedBars data={stats.geography?.languages ?? []} color={LBX.green} limit={15} />
          </Card>
          <Card title="Country spread" subtitle="How far your watching ranges">
            <CategoryRadar
              data={stats.geography?.countries ?? []}
              color={LBX.orange}
            />
          </Card>
          <Card title="Language spread" subtitle="Beyond the dominant one">
            <CategoryRadar
              data={(stats.geography?.languages ?? []).slice(1)}
              color={LBX.blue}
            />
          </Card>
        </div>
      )}
    </div>
  )
}
