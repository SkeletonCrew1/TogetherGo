import { Link } from 'react-router-dom'

import { Avatar } from '@/components/ui/Avatar'
import { Badge } from '@/components/ui/Badge'
import { RatingStars } from '@/components/ui/Stars'
import {
  CATEGORY_LABELS,
  CATEGORY_STYLES,
  TRIP_STATUS_LABELS,
  TRIP_STATUS_STYLES,
  durationDays,
  formatDateRange,
} from '@/lib/format'
import type { TripListItem } from '@/lib/types'

/** "Kraków → Zakopane → Morskie Oko", with an ellipsis when it is a prefix. */
function RouteSummaryLine({ item }: { item: TripListItem }) {
  const { points, truncated, total_points } = item.route_summary
  const names = points.length > 0 ? points : [item.departure.name, item.destination.name]

  return (
    <p className="truncate text-sm text-slate-600" title={names.join(' → ')}>
      {names.join(' → ')}
      {/* The API sends `truncated` as a boolean rather than appending "…" to
          the list, so that a stop legitimately named "…" is distinguishable
          from the marker. This is where the punctuation gets added. */}
      {truncated && <span className="text-slate-400"> … +{total_points - points.length}</span>}
    </p>
  )
}

export function TripCard({
  item,
  footer,
}: {
  item: TripListItem
  /** Extra controls a screen wants on the card — /my-trips uses this. */
  footer?: React.ReactNode
}) {
  const days = durationDays(item.start_at, item.end_at)

  return (
    <article className="rounded-xl border border-slate-200 bg-white transition-shadow hover:shadow-md">
      <Link to={`/trip/${item.id}`} className="block p-4">
        <div className="flex items-start justify-between gap-3">
          <h3 className="line-clamp-2 font-semibold text-slate-900">{item.title}</h3>
          <div className="flex shrink-0 flex-col items-end gap-1">
            <Badge className={CATEGORY_STYLES[item.category]}>
              {CATEGORY_LABELS[item.category]}
            </Badge>
            {item.status !== 'recruiting' && (
              <Badge className={TRIP_STATUS_STYLES[item.status]}>
                {TRIP_STATUS_LABELS[item.status]}
              </Badge>
            )}
          </div>
        </div>

        <div className="mt-2 space-y-1">
          <RouteSummaryLine item={item} />
          <p className="text-sm text-slate-500">
            {formatDateRange(item.start_at, item.end_at)}
            <span className="text-slate-400"> · {days === 1 ? '1 day' : `${days} days`}</span>
          </p>
        </div>

        <div className="mt-4 flex items-center gap-2.5 border-t border-slate-100 pt-3">
          <Avatar
            name={item.organizer.full_name}
            photoUrl={item.organizer.photo_url}
            size="sm"
          />
          <div className="min-w-0">
            <p className="truncate text-sm font-medium text-slate-800">
              {/* Every organizer field but the id is nullable, and that is the
                  documented degraded mode when trip cannot reach identity —
                  the card renders rather than the page failing. */}
              {item.organizer.full_name ?? 'Unknown organizer'}
            </p>
            <RatingStars value={item.organizer.rating_avg} size="sm" />
          </div>

          <span
            className="ml-auto shrink-0 text-sm font-medium text-slate-700"
            title={`${item.approved_count} of ${item.capacity} seats taken`}
          >
            <span aria-hidden>👥 </span>
            {item.approved_count}/{item.capacity}
            <span className="sr-only"> people, {item.free_slots} free</span>
          </span>
        </div>
      </Link>

      {footer && <div className="border-t border-slate-100 px-4 py-3">{footer}</div>}
    </article>
  )
}
