import { TransportIcon, transportLabel } from './transport'
import { formatDateTime, formatTime } from '@/lib/format'
import { cx } from '@/lib/format'
import type { RoutePoint } from '@/lib/types'

/**
 * The plan, as an ordered timeline.
 *
 * Times come from `arrive_at`, which is nullable per stop: the organizer may
 * have planned the first three and left the rest open, and a stop with no time
 * is shown without one rather than with a made-up one.
 *
 * The transport icon sits on the connector between two stops, not on the stop
 * itself — `transport` on a point means how you get *to* it, so drawing it in
 * the gap above is where it actually belongs.
 */
export function Timeline({
  points,
  selectedId,
  onSelect,
}: {
  points: RoutePoint[]
  selectedId?: string | null
  onSelect?: (pointId: string) => void
}) {
  const ordered = [...points].sort((a, b) => a.seq - b.seq)

  // A one-day trip shows times only; a multi-day one has to show the date too,
  // or "09:00" on stop 1 and "09:00" on stop 6 read as the same moment.
  const days = new Set(
    ordered
      .filter((point) => point.arrive_at)
      .map((point) => new Date(point.arrive_at as string).toDateString()),
  )
  const sameDay = days.size <= 1

  return (
    <ol className="relative">
      {ordered.map((point, index) => {
        const isFirst = index === 0
        const isLast = index === ordered.length - 1
        const selected = selectedId === point.id

        return (
          <li key={point.id} className="relative pb-6 pl-11 last:pb-0">
            {/* The vertical rail, stopped short on the final stop so the line
                does not dangle past the end of the route. */}
            {!isLast && (
              <span
                aria-hidden
                className="absolute left-[13px] top-7 h-full w-0.5 bg-slate-200"
              />
            )}

            <span
              aria-hidden
              className={cx(
                'absolute left-0 top-0 flex h-7 w-7 items-center justify-center rounded-full text-xs font-semibold text-white ring-2 ring-white',
                isFirst ? 'bg-emerald-600' : isLast ? 'bg-rose-600' : 'bg-brand-600',
              )}
            >
              {index + 1}
            </span>

            {!isFirst && point.transport && (
              <span className="absolute left-[3px] top-[-14px] flex h-6 w-6 items-center justify-center rounded-full bg-white text-slate-500 ring-1 ring-slate-200">
                <TransportIcon mode={point.transport} className="h-3.5 w-3.5" />
              </span>
            )}

            <button
              type="button"
              onClick={onSelect ? () => onSelect(point.id) : undefined}
              disabled={!onSelect}
              className={cx(
                'w-full rounded-lg px-3 py-2 text-left transition-colors',
                onSelect && 'hover:bg-slate-50',
                selected && 'bg-brand-50 ring-1 ring-brand-200',
                !onSelect && 'cursor-default',
              )}
            >
              <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                <span className="font-medium text-slate-900">{point.name}</span>
                {point.arrive_at && (
                  <time
                    dateTime={point.arrive_at}
                    className="text-sm tabular-nums text-slate-500"
                  >
                    {sameDay ? formatTime(point.arrive_at) : formatDateTime(point.arrive_at)}
                  </time>
                )}
              </div>
              {point.transport && (
                <span className="mt-0.5 block text-xs text-slate-500">
                  by {transportLabel(point.transport)}
                </span>
              )}
            </button>
          </li>
        )
      })}
    </ol>
  )
}
