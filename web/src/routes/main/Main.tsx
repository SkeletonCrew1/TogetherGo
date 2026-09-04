import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'

import { EMPTY_FILTERS, FilterSidebar, activeFilterCount, type FilterState } from './FilterSidebar'
import { TripCard } from '@/components/trip/TripCard'
import { Alert } from '@/components/ui/Alert'
import { Button } from '@/components/ui/Button'
import { EmptyState } from '@/components/ui/EmptyState'
import { LoadingRegion, TripCardSkeleton } from '@/components/ui/Skeleton'
import { Spinner } from '@/components/ui/Spinner'
import { useDebounced } from '@/hooks/useDebounced'
import { useInfiniteScroll } from '@/hooks/useInfiniteScroll'
import { useTripSearch } from '@/hooks/useTripSearch'
import { errorMessage } from '@/lib/api/errors'
import type { SearchFilter } from '@/lib/types'

/**
 * The sidebar's strings become the API's query parameters here, and only here.
 *
 * Two details the trip service cares about:
 *  - The endpoint's parameter allowlist is closed, so an empty value must be
 *    omitted rather than sent empty — `?min_days=` is a 400.
 *  - Dates arrive from `<input type="date">` as a bare day. `date_from` becomes
 *    the start of that day and `date_to` the end of it, because "to the 15th"
 *    means including the 15th, and sending midnight would exclude everything
 *    that day.
 */
export function toSearchFilter(state: FilterState, query: string): SearchFilter {
  const filter: SearchFilter = {}

  if (query.trim()) filter.q = query.trim()
  if (state.dateFrom) filter.date_from = new Date(`${state.dateFrom}T00:00:00`).toISOString()
  if (state.dateTo) filter.date_to = new Date(`${state.dateTo}T23:59:59`).toISOString()

  if (state.near) {
    filter.near_lat = state.near.lat
    filter.near_lng = state.near.lng
    filter.radius_km = state.radiusKm
  }

  const minDays = Number.parseInt(state.minDays, 10)
  if (Number.isFinite(minDays)) filter.min_days = minDays
  const maxDays = Number.parseInt(state.maxDays, 10)
  if (Number.isFinite(maxDays)) filter.max_days = maxDays
  const minFreeSlots = Number.parseInt(state.minFreeSlots, 10)
  if (Number.isFinite(minFreeSlots)) filter.min_free_slots = minFreeSlots

  if (state.categories.length > 0) filter.categories = [...state.categories].sort()

  return filter
}

export function Main() {
  const [query, setQuery] = useState('')
  const [filters, setFilters] = useState<FilterState>(EMPTY_FILTERS)
  const [sidebarOpen, setSidebarOpen] = useState(false)

  // Debounced together: dragging the radius slider is as much a burst of
  // requests as typing is.
  const debouncedQuery = useDebounced(query)
  const debouncedFilters = useDebounced(filters)

  const searchFilter = useMemo(
    () => toSearchFilter(debouncedFilters, debouncedQuery),
    [debouncedFilters, debouncedQuery],
  )

  const {
    data,
    error,
    isLoading,
    isFetching,
    isFetchingNextPage,
    hasNextPage,
    fetchNextPage,
    refetch,
  } = useTripSearch(searchFilter)

  const items = useMemo(() => data?.pages.flatMap((page) => page.items) ?? [], [data])

  const sentinelRef = useInfiniteScroll(
    () => {
      // Guarded here rather than by unmounting the sentinel, so that a page
      // which does not fill the viewport still loads the next one.
      if (hasNextPage && !isFetchingNextPage) void fetchNextPage()
    },
    hasNextPage && !isFetchingNextPage,
  )

  const activeCount = activeFilterCount(filters)

  return (
    <div className="mx-auto max-w-7xl px-4 py-6">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
        <div className="relative flex-1">
          <label htmlFor="trip-search" className="sr-only">
            Search trips
          </label>
          <input
            id="trip-search"
            type="search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Search trips — a place, a title, an activity…"
            maxLength={200}
            className="w-full rounded-xl border-0 bg-white py-2.5 pl-10 pr-3 text-slate-900 shadow-sm ring-1 ring-inset ring-slate-300 placeholder:text-slate-400 focus:ring-2 focus:ring-inset focus:ring-brand-600"
          />
          <span aria-hidden className="pointer-events-none absolute left-3.5 top-2.5 text-slate-400">
            🔍
          </span>
        </div>

        <div className="flex gap-2">
          <Button
            variant="secondary"
            className="lg:hidden"
            onClick={() => setSidebarOpen((open) => !open)}
            aria-expanded={sidebarOpen}
          >
            Filters{activeCount > 0 && ` (${activeCount})`}
          </Button>
          <Link to="/organize-trip">
            <Button>Organize trip</Button>
          </Link>
        </div>
      </div>

      <div className="mt-6 grid gap-6 lg:grid-cols-[19rem,1fr]">
        <aside className={sidebarOpen ? 'block' : 'hidden lg:block'}>
          <div className="lg:sticky lg:top-20">
            <FilterSidebar
              value={filters}
              onChange={setFilters}
              onReset={() => setFilters(EMPTY_FILTERS)}
            />
          </div>
        </aside>

        <section aria-label="Trips">
          {/* A refetch caused by a changed filter keeps the old list on screen
              and marks it stale, rather than flashing skeletons on every
              keystroke. Only the very first load gets skeletons. */}
          {isLoading ? (
            <LoadingRegion label="Loading trips">
              <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
                {Array.from({ length: 6 }, (_, index) => (
                  <TripCardSkeleton key={index} />
                ))}
              </div>
            </LoadingRegion>
          ) : error ? (
            <Alert>
              <p>{errorMessage(error)}</p>
              <Button variant="secondary" size="sm" className="mt-3" onClick={() => void refetch()}>
                Try again
              </Button>
            </Alert>
          ) : items.length === 0 ? (
            <EmptyState
              title={activeCount > 0 || debouncedQuery ? 'No trips match that' : 'No trips yet'}
              description={
                activeCount > 0 || debouncedQuery
                  ? 'Try widening the radius, clearing a date, or removing a category.'
                  : 'Only trips that are recruiting and have not started yet appear here. Be the first to organize one.'
              }
              action={
                activeCount > 0 || debouncedQuery ? (
                  <Button
                    variant="secondary"
                    onClick={() => {
                      setFilters(EMPTY_FILTERS)
                      setQuery('')
                    }}
                  >
                    Clear filters
                  </Button>
                ) : (
                  <Link to="/organize-trip">
                    <Button>Organize a trip</Button>
                  </Link>
                )
              }
            />
          ) : (
            <>
              <div
                className={
                  isFetching && !isFetchingNextPage
                    ? 'grid gap-4 opacity-60 transition-opacity sm:grid-cols-2 xl:grid-cols-3'
                    : 'grid gap-4 sm:grid-cols-2 xl:grid-cols-3'
                }
              >
                {items.map((item) => (
                  <TripCard key={item.id} item={item} />
                ))}
              </div>

              {/* The sentinel and the fallback button. The button is not a
                  progressive-enhancement nicety: an IntersectionObserver never
                  fires for a user who reached the bottom with Ctrl+End. */}
              <div ref={sentinelRef} className="h-px" aria-hidden />

              <div className="mt-6 flex justify-center">
                {isFetchingNextPage ? (
                  <Spinner />
                ) : hasNextPage ? (
                  <Button variant="secondary" onClick={() => void fetchNextPage()}>
                    Load more
                  </Button>
                ) : (
                  <p className="text-sm text-slate-400">That is every matching trip.</p>
                )}
              </div>
            </>
          )}
        </section>
      </div>
    </div>
  )
}
