import { useMemo } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useMutation, useQueryClient } from '@tanstack/react-query'

import { JoinRequestQueue } from '@/components/trip/JoinRequestQueue'
import { TripCard } from '@/components/trip/TripCard'
import { Alert } from '@/components/ui/Alert'
import { Badge } from '@/components/ui/Badge'
import { Button } from '@/components/ui/Button'
import { EmptyState } from '@/components/ui/EmptyState'
import { LoadingRegion, TripCardSkeleton } from '@/components/ui/Skeleton'
import { Spinner } from '@/components/ui/Spinner'
import { Tabs } from '@/components/ui/Tabs'
import { keys } from '@/hooks/queryKeys'
import { useInfiniteScroll } from '@/hooks/useInfiniteScroll'
import { useMyTrips } from '@/hooks/useMyTrips'
import * as participationApi from '@/lib/api/participation'
import * as tripsApi from '@/lib/api/trips'
import { errorMessage } from '@/lib/api/errors'
import { JOIN_STATUS_LABELS, TRIP_STATUS_LABELS, TRIP_STATUS_STYLES } from '@/lib/format'
import type { MyTripItem } from '@/lib/types'

type TabId = 'organized' | 'participate'

const ROLE: Record<TabId, 'organizer' | 'participant'> = {
  organized: 'organizer',
  participate: 'participant',
}

export function MyTrips() {
  // The tab lives in the URL: /my-trips is a screen people link to and reload,
  // and the back button should step between tabs the way it looks like it will.
  const [params, setParams] = useSearchParams()
  const active: TabId = params.get('tab') === 'participate' ? 'participate' : 'organized'

  const query = useMyTrips(ROLE[active])
  const items = useMemo(
    () => query.data?.pages.flatMap((page) => page.items) ?? [],
    [query.data],
  )

  const sentinelRef = useInfiniteScroll(
    () => {
      if (query.hasNextPage && !query.isFetchingNextPage) void query.fetchNextPage()
    },
    query.hasNextPage === true && !query.isFetchingNextPage,
  )

  return (
    <div className="mx-auto max-w-5xl px-4 py-6">
      <div className="flex items-center justify-between gap-4">
        <h1 className="text-2xl font-semibold text-slate-900">My trips</h1>
        <Link to="/organize-trip">
          <Button>Organize trip</Button>
        </Link>
      </div>

      <div className="mt-5">
        <Tabs<TabId>
          active={active}
          onChange={(tab) => setParams(tab === 'organized' ? {} : { tab }, { replace: true })}
          tabs={[
            { id: 'organized', label: 'I organized' },
            { id: 'participate', label: 'I participate' },
          ]}
        />
      </div>

      <div className="mt-6">
        {query.isLoading ? (
          <LoadingRegion label="Loading your trips">
            <div className="space-y-4">
              <TripCardSkeleton />
              <TripCardSkeleton />
            </div>
          </LoadingRegion>
        ) : query.error ? (
          <Alert>
            <p>{errorMessage(query.error)}</p>
            <Button
              variant="secondary"
              size="sm"
              className="mt-3"
              onClick={() => void query.refetch()}
            >
              Try again
            </Button>
          </Alert>
        ) : items.length === 0 ? (
          <EmptyState
            title={active === 'organized' ? 'You have not organized a trip yet' : 'You have not joined a trip yet'}
            description={
              active === 'organized'
                ? 'Draft one and publish it when it is ready — a draft is private until you do.'
                : 'Find something you like and ask its organizer to let you come along.'
            }
            action={
              <Link to={active === 'organized' ? '/organize-trip' : '/main'}>
                <Button>{active === 'organized' ? 'Organize a trip' : 'Find trips'}</Button>
              </Link>
            }
          />
        ) : (
          <>
            <div className="space-y-4">
              {items.map((item) =>
                active === 'organized' ? (
                  <OrganizedTrip key={item.id} item={item} />
                ) : (
                  <ParticipatingTrip key={item.id} item={item} />
                ),
              )}
            </div>

            <div ref={sentinelRef} className="h-px" aria-hidden />

            <div className="mt-6 flex justify-center">
              {query.isFetchingNextPage ? (
                <Spinner />
              ) : query.hasNextPage ? (
                <Button variant="secondary" onClick={() => void query.fetchNextPage()}>
                  Load more
                </Button>
              ) : null}
            </div>
          </>
        )}
      </div>
    </div>
  )
}

/** An organized trip, with its pending applications inline and approvable. */
function OrganizedTrip({ item }: { item: MyTripItem }) {
  const queryClient = useQueryClient()

  const publishMutation = useMutation({
    mutationFn: () => tripsApi.publish(item.id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: keys.myTrips.all }),
  })

  return (
    <TripCard
      item={item}
      footer={
        <div className="space-y-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h4 className="text-sm font-semibold text-slate-700">
              Join requests
              {(item.pending_requests_count ?? 0) > 0 && (
                <Badge className="ml-2 bg-brand-600 text-white">
                  {item.pending_requests_count}
                </Badge>
              )}
            </h4>

            <div className="flex gap-2">
              {item.status === 'draft' && (
                <Button
                  size="sm"
                  isLoading={publishMutation.isPending}
                  onClick={() => publishMutation.mutate()}
                >
                  Publish
                </Button>
              )}
              <Link to={`/chat/${item.id}`}>
                <Button size="sm" variant="secondary">
                  Chat
                </Button>
              </Link>
            </div>
          </div>

          {publishMutation.error && <Alert>{errorMessage(publishMutation.error)}</Alert>}

          {/* A draft has no requests by construction — nobody can see it. */}
          {item.status === 'draft' ? (
            <p className="text-sm text-slate-500">
              This trip is a draft, so nobody can find it or ask to join yet.
            </p>
          ) : (
            <JoinRequestQueue tripId={item.id} />
          )}
        </div>
      }
    />
  )
}

/** A trip the caller is on, or has applied to, with a status badge and a way out. */
function ParticipatingTrip({ item }: { item: MyTripItem }) {
  const queryClient = useQueryClient()

  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: keys.myTrips.all }),
      queryClient.invalidateQueries({ queryKey: keys.trips.all }),
    ])
  }

  const withdrawMutation = useMutation({
    mutationFn: () => participationApi.cancelMyRequest(item.id),
    onSuccess: refresh,
  })

  const leaveMutation = useMutation({
    mutationFn: () => participationApi.leave(item.id),
    onSuccess: refresh,
  })

  // An unanswered application is shown as "Requested" rather than as the trip's
  // own status: the trip is recruiting, but what matters to this person is that
  // nobody has answered them yet.
  const isPending = item.membership_status === 'requested'
  const badge = isPending
    ? { label: JOIN_STATUS_LABELS.pending, style: 'bg-amber-100 text-amber-900' }
    : { label: TRIP_STATUS_LABELS[item.status], style: TRIP_STATUS_STYLES[item.status] }

  const error = withdrawMutation.error ?? leaveMutation.error

  return (
    <TripCard
      item={item}
      footer={
        <div className="space-y-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <Badge className={badge.style}>{badge.label}</Badge>

            <div className="flex gap-2">
              {!isPending && (
                <Link to={`/chat/${item.id}`}>
                  <Button size="sm" variant="secondary">
                    Chat
                  </Button>
                </Link>
              )}

              {isPending ? (
                <Button
                  size="sm"
                  variant="ghost"
                  isLoading={withdrawMutation.isPending}
                  onClick={() => withdrawMutation.mutate()}
                >
                  Withdraw request
                </Button>
              ) : item.status === 'recruiting' || item.status === 'in_progress' ? (
                <Button
                  size="sm"
                  variant="ghost"
                  isLoading={leaveMutation.isPending}
                  onClick={() => leaveMutation.mutate()}
                >
                  Cancel
                </Button>
              ) : null}
            </div>
          </div>

          {error && <Alert>{errorMessage(error)}</Alert>}
        </div>
      }
    />
  )
}
