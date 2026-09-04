import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { RouteMap } from '@/components/map/RouteMap'
import { Timeline } from '@/components/trip/Timeline'
import { TripCard } from '@/components/trip/TripCard'
import { Alert } from '@/components/ui/Alert'
import { Avatar } from '@/components/ui/Avatar'
import { Badge } from '@/components/ui/Badge'
import { Button } from '@/components/ui/Button'
import { EmptyState } from '@/components/ui/EmptyState'
import { Textarea } from '@/components/ui/Field'
import { Modal } from '@/components/ui/Modal'
import { LoadingRegion, Skeleton, SkeletonText } from '@/components/ui/Skeleton'
import { RatingStars } from '@/components/ui/Stars'
import { useCurrentUser } from '@/auth/AuthContext'
import { useMyJoinRequest } from '@/hooks/useMyJoinRequest'
import { useParticipantProfiles } from '@/hooks/useParticipantProfiles'
import { keys } from '@/hooks/queryKeys'
import * as participationApi from '@/lib/api/participation'
import * as tripsApi from '@/lib/api/trips'
import { ApiError, errorMessage } from '@/lib/api/errors'
import {
  CATEGORY_LABELS,
  CATEGORY_STYLES,
  TRIP_STATUS_LABELS,
  TRIP_STATUS_STYLES,
  durationDays,
  formatDateRange,
} from '@/lib/format'
import type { PublicProfile, Trip } from '@/lib/types'

export function TripDetail() {
  const { id = '' } = useParams()
  const me = useCurrentUser()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const [selectedPointId, setSelectedPointId] = useState<string | null>(null)
  const [joinOpen, setJoinOpen] = useState(false)
  const [joinMessage, setJoinMessage] = useState('')
  const [actionError, setActionError] = useState<string | null>(null)

  const tripQuery = useQuery({
    queryKey: keys.trips.detail(id),
    queryFn: () => tripsApi.get(id),
    enabled: Boolean(id),
  })

  const trip = tripQuery.data
  const isOrganizer = trip?.organizer_id === me.id
  const isParticipant = Boolean(
    trip?.participants.some((participant) => participant.user_id === me.id),
  )

  // Only worth asking when the answer can change the button: an organizer and a
  // seated participant already know where they stand.
  const myRequestQuery = useMyJoinRequest(id, Boolean(trip) && !isOrganizer && !isParticipant)

  const similarQuery = useQuery({
    queryKey: keys.trips.similar(id),
    queryFn: () => tripsApi.similar(id),
    enabled: Boolean(trip),
    staleTime: 5 * 60_000,
  })

  const { profiles } = useParticipantProfiles(
    trip?.participants.map((participant) => participant.user_id) ?? [],
  )

  /**
   * Every mutation on this screen invalidates the same two families, because
   * every one of them changes both the trip's seat count and the caller's own
   * standing on it.
   */
  const refreshTrip = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: keys.trips.detail(id) }),
      queryClient.invalidateQueries({ queryKey: keys.requests.mine(id) }),
      queryClient.invalidateQueries({ queryKey: keys.myTrips.all }),
      // Free-slot counts on every search page are now stale.
      queryClient.invalidateQueries({ queryKey: keys.trips.all }),
    ])
  }

  const joinMutation = useMutation({
    mutationFn: () => participationApi.requestToJoin(id, joinMessage),
    onSuccess: async () => {
      setJoinOpen(false)
      setJoinMessage('')
      setActionError(null)
      await refreshTrip()
    },
    onError: (error) => setActionError(describeJoinError(error)),
  })

  const cancelRequestMutation = useMutation({
    mutationFn: () => participationApi.cancelMyRequest(id),
    onSuccess: refreshTrip,
    onError: (error) => setActionError(errorMessage(error)),
  })

  const leaveMutation = useMutation({
    mutationFn: () => participationApi.leave(id),
    onSuccess: refreshTrip,
    onError: (error) => setActionError(errorMessage(error)),
  })

  const publishMutation = useMutation({
    mutationFn: () => tripsApi.publish(id),
    onSuccess: refreshTrip,
    onError: (error) => setActionError(errorMessage(error)),
  })

  const cancelTripMutation = useMutation({
    mutationFn: () => tripsApi.cancel(id),
    onSuccess: refreshTrip,
    onError: (error) => setActionError(errorMessage(error)),
  })

  if (tripQuery.isLoading) return <TripDetailSkeleton />

  if (tripQuery.error) {
    const notFound = tripQuery.error instanceof ApiError && tripQuery.error.status === 404
    return (
      <div className="mx-auto max-w-3xl px-4 py-12">
        <EmptyState
          title={notFound ? 'That trip is not here' : 'Could not load this trip'}
          description={
            notFound
              ? // A draft answers 404 to everyone but its organizer, deliberately:
                // a 403 would confirm the id is real.
                'It may have been deleted, or it may be a draft that only its organizer can see.'
              : errorMessage(tripQuery.error)
          }
          action={
            <Link to="/main">
              <Button variant="secondary">Back to trips</Button>
            </Link>
          }
        />
      </div>
    )
  }

  if (!trip) return null

  const organizer = profiles.get(trip.organizer_id) ?? null
  const days = durationDays(trip.start_at, trip.end_at)

  return (
    <div className="mx-auto max-w-6xl px-4 py-6">
      <Link to="/main" className="text-sm text-slate-500 hover:text-slate-800">
        ← All trips
      </Link>

      <header className="mt-3 flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <Badge className={CATEGORY_STYLES[trip.category]}>
              {CATEGORY_LABELS[trip.category]}
            </Badge>
            <Badge className={TRIP_STATUS_STYLES[trip.status]}>
              {TRIP_STATUS_LABELS[trip.status]}
            </Badge>
          </div>
          <h1 className="mt-2 text-2xl font-semibold text-slate-900">{trip.title}</h1>
          <p className="mt-1 text-slate-600">
            {formatDateRange(trip.start_at, trip.end_at)}
            <span className="text-slate-400"> · {days === 1 ? '1 day' : `${days} days`}</span>
            <span className="text-slate-400">
              {' '}
              · {trip.approved_count}/{trip.capacity} people
            </span>
          </p>
        </div>

        <ActionButton
          trip={trip}
          isOrganizer={isOrganizer}
          isParticipant={isParticipant}
          requestStatus={myRequestQuery.data?.status ?? null}
          isResolvingRequest={myRequestQuery.isLoading}
          onRequest={() => setJoinOpen(true)}
          onWithdraw={() => cancelRequestMutation.mutate()}
          onLeave={() => leaveMutation.mutate()}
          onPublish={() => publishMutation.mutate()}
          onCancelTrip={() => cancelTripMutation.mutate()}
          onManage={() => navigate('/my-trips?tab=organized')}
          busy={
            joinMutation.isPending ||
            cancelRequestMutation.isPending ||
            leaveMutation.isPending ||
            publishMutation.isPending ||
            cancelTripMutation.isPending
          }
        />
      </header>

      {actionError && (
        <Alert className="mt-4">
          {actionError}
          <button
            type="button"
            onClick={() => setActionError(null)}
            className="ml-2 underline underline-offset-2"
          >
            Dismiss
          </button>
        </Alert>
      )}

      {myRequestQuery.data?.status === 'rejected' && myRequestQuery.data.reason && (
        <Alert tone="warning" className="mt-4">
          <span className="font-medium">The organizer declined:</span>{' '}
          {myRequestQuery.data.reason}
        </Alert>
      )}

      <div className="mt-6 grid gap-6 lg:grid-cols-[1fr,20rem]">
        <div className="space-y-6">
          {trip.description && (
            <section className="rounded-xl border border-slate-200 bg-white p-5">
              <h2 className="mb-2 text-sm font-semibold uppercase tracking-wide text-slate-500">
                About this trip
              </h2>
              <p className="whitespace-pre-wrap text-slate-700">{trip.description}</p>
            </section>
          )}

          <section className="overflow-hidden rounded-xl border border-slate-200 bg-white">
            <h2 className="border-b border-slate-100 px-5 py-3 text-sm font-semibold uppercase tracking-wide text-slate-500">
              Route
            </h2>
            <RouteMap
              points={trip.points}
              className="h-80 w-full"
              selectedId={selectedPointId}
              onSelect={setSelectedPointId}
            />
          </section>

          <section className="rounded-xl border border-slate-200 bg-white p-5">
            <h2 className="mb-4 text-sm font-semibold uppercase tracking-wide text-slate-500">
              Plan
            </h2>
            <Timeline
              points={trip.points}
              selectedId={selectedPointId}
              onSelect={setSelectedPointId}
            />
          </section>

          <section>
            <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-slate-500">
              Similar trips
            </h2>
            {similarQuery.isLoading ? (
              <Skeleton className="h-32 w-full" />
            ) : similarQuery.data && similarQuery.data.items.length > 0 ? (
              <div className="grid gap-4 sm:grid-cols-2">
                {similarQuery.data.items.map((item) => (
                  <TripCard key={item.id} item={item} />
                ))}
              </div>
            ) : (
              <p className="text-sm text-slate-500">
                Nothing else like this is recruiting right now.
              </p>
            )}
          </section>
        </div>

        <aside className="space-y-6">
          <section className="rounded-xl border border-slate-200 bg-white p-5">
            <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-slate-500">
              Organizer
            </h2>
            <PersonRow userId={trip.organizer_id} profile={organizer} subtitle="Organizer" />
          </section>

          <section className="rounded-xl border border-slate-200 bg-white p-5">
            <div className="mb-3 flex items-baseline justify-between">
              <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
                Travellers
              </h2>
              <span className="text-sm text-slate-500">
                {trip.approved_count}/{trip.capacity}
              </span>
            </div>

            <ul className="space-y-3">
              {trip.participants.map((participant) => (
                <li key={participant.user_id}>
                  <PersonRow
                    userId={participant.user_id}
                    profile={profiles.get(participant.user_id) ?? null}
                    subtitle={participant.role === 'organizer' ? 'Organizer' : undefined}
                    isYou={participant.user_id === me.id}
                  />
                </li>
              ))}
            </ul>

            {trip.spots_left > 0 && (
              <p className="mt-4 text-sm text-slate-500">
                {trip.spots_left === 1 ? '1 seat left' : `${trip.spots_left} seats left`}
              </p>
            )}
          </section>

          <section className="rounded-xl border border-slate-200 bg-white p-5">
            <h2 className="mb-2 text-sm font-semibold uppercase tracking-wide text-slate-500">
              Recommendations
            </h2>
            <Recommendations trip={trip} />
          </section>
        </aside>
      </div>

      <Modal
        open={joinOpen}
        onClose={() => setJoinOpen(false)}
        title="Request to join"
        footer={
          <>
            <Button variant="secondary" onClick={() => setJoinOpen(false)}>
              Cancel
            </Button>
            <Button isLoading={joinMutation.isPending} onClick={() => joinMutation.mutate()}>
              Send request
            </Button>
          </>
        }
      >
        <p className="mb-3 text-sm text-slate-600">
          The organizer decides who joins. A sentence about yourself helps.
        </p>
        <Textarea
          label="Message to the organizer"
          rows={4}
          maxLength={1000}
          value={joinMessage}
          onChange={(event) => setJoinMessage(event.target.value)}
          placeholder="Why you want to come, what you bring, how you travel…"
          hint="Optional."
        />
      </Modal>
    </div>
  )
}

/**
 * The context-dependent action.
 *
 * The order of these branches is the whole logic, so it is one function rather
 * than a scatter of ternaries in the header: organizer first (they can never
 * join their own trip), then a seated participant, then whatever the caller's
 * join request says, then the trip's own status.
 */
function ActionButton({
  trip,
  isOrganizer,
  isParticipant,
  requestStatus,
  isResolvingRequest,
  onRequest,
  onWithdraw,
  onLeave,
  onPublish,
  onCancelTrip,
  onManage,
  busy,
}: {
  trip: Trip
  isOrganizer: boolean
  isParticipant: boolean
  requestStatus: string | null
  isResolvingRequest: boolean
  onRequest: () => void
  onWithdraw: () => void
  onLeave: () => void
  onPublish: () => void
  onCancelTrip: () => void
  onManage: () => void
  busy: boolean
}) {
  const chat = (
    <Link to={`/chat/${trip.id}`}>
      <Button variant="secondary">Chat</Button>
    </Link>
  )

  if (isOrganizer) {
    return (
      <div className="flex flex-wrap gap-2">
        {trip.status === 'draft' && (
          <Button isLoading={busy} onClick={onPublish}>
            Publish
          </Button>
        )}
        {(trip.status === 'recruiting' || trip.status === 'in_progress') && (
          <>
            {chat}
            <Button variant="secondary" onClick={onManage}>
              Manage requests
            </Button>
            <Button variant="danger" isLoading={busy} onClick={onCancelTrip}>
              Cancel trip
            </Button>
          </>
        )}
        {(trip.status === 'completed' || trip.status === 'cancelled') && chat}
      </div>
    )
  }

  if (isParticipant) {
    return (
      <div className="flex flex-wrap gap-2">
        {chat}
        {/* Leaving is only meaningful while the trip is still ahead. Once it is
            under way or finished the seat is history. */}
        {(trip.status === 'recruiting' || trip.status === 'in_progress') && (
          <Button variant="secondary" isLoading={busy} onClick={onLeave}>
            Leave
          </Button>
        )}
      </div>
    )
  }

  if (isResolvingRequest) {
    return <Button disabled>Checking…</Button>
  }

  if (requestStatus === 'pending') {
    return (
      <div className="flex flex-wrap items-center gap-2">
        <Button disabled>Requested</Button>
        <Button variant="ghost" isLoading={busy} onClick={onWithdraw}>
          Withdraw
        </Button>
      </div>
    )
  }

  if (trip.status !== 'recruiting') {
    return (
      <Button disabled title={`This trip is ${TRIP_STATUS_LABELS[trip.status].toLowerCase()}`}>
        Not accepting requests
      </Button>
    )
  }

  if (trip.spots_left === 0) {
    return <Button disabled>Trip is full</Button>
  }

  // A rejected or withdrawn request may be replaced by a new one; the service
  // decides whether it will accept it, and answers `duplicate_request` if not.
  return (
    <Button isLoading={busy} onClick={onRequest}>
      {requestStatus === 'rejected' ? 'Request again' : 'Request to join'}
    </Button>
  )
}

function PersonRow({
  userId,
  profile,
  subtitle,
  isYou = false,
}: {
  userId: string
  profile: PublicProfile | null
  subtitle?: string
  isYou?: boolean
}) {
  if (!profile) {
    // Identity is unreachable or the account is gone. The roster still renders
    // — the same degraded mode the API itself uses for organizer blocks.
    return (
      <div className="flex items-center gap-3">
        <Avatar name={null} size="sm" />
        <div className="min-w-0">
          <p className="truncate text-sm text-slate-500">Traveller</p>
          <p className="truncate font-mono text-xs text-slate-400">{userId.slice(0, 8)}</p>
        </div>
      </div>
    )
  }

  return (
    <div className="flex items-center gap-3">
      <Avatar name={profile.full_name} photoUrl={profile.photo_url} size="sm" />
      <div className="min-w-0">
        <p className="truncate text-sm font-medium text-slate-900">
          {profile.full_name}
          {isYou && <span className="ml-1 font-normal text-slate-400">(you)</span>}
        </p>
        <div className="flex items-center gap-2">
          <RatingStars value={profile.rating_avg} count={profile.rating_count} size="sm" />
        </div>
        {subtitle && <p className="text-xs text-slate-400">{subtitle}</p>}
      </div>
    </div>
  )
}

/**
 * Practical notes derived from the trip itself.
 *
 * There is no recommendations field on the wire and no service that produces
 * one, so this states what the data actually supports rather than inventing
 * advice. When a recommendations endpoint exists, this is the component it
 * replaces.
 */
function Recommendations({ trip }: { trip: Trip }) {
  const days = durationDays(trip.start_at, trip.end_at)
  const modes = Array.from(
    new Set(trip.points.map((point) => point.transport).filter(Boolean) as string[]),
  )
  const untimed = trip.points.filter((point) => !point.arrive_at).length

  const notes = [
    `${trip.points.length} stops over ${days === 1 ? 'one day' : `${days} days`}.`,
    modes.length > 0
      ? `Getting around by ${modes.join(', ')} — pack for it.`
      : 'The organizer has not said how you get between stops; worth asking.',
    untimed > 0
      ? `${untimed} of the stops have no planned time, so the pace is flexible.`
      : 'Every stop has a planned time — this one runs to a schedule.',
    trip.category === 'abroad'
      ? 'Crossing a border: check what documents you need.'
      : trip.category === 'hiking'
        ? 'Boots, water, and something warm for the top.'
        : null,
  ].filter(Boolean) as string[]

  return (
    <ul className="space-y-2 text-sm text-slate-600">
      {notes.map((note) => (
        <li key={note} className="flex gap-2">
          <span aria-hidden className="text-slate-300">•</span>
          {note}
        </li>
      ))}
    </ul>
  )
}

/** The service's codes, turned into sentences that say what to do next. */
function describeJoinError(error: unknown): string {
  if (!(error instanceof ApiError)) return errorMessage(error)
  switch (error.code) {
    case 'trip_full':
      return 'The last seat went while you were reading. Nothing to do here.'
    case 'duplicate_request':
      return 'You have already applied to this trip.'
    case 'trip_not_recruiting':
      return 'This trip is no longer taking requests.'
    case 'organizer_cannot_join':
      return 'You are the organizer of this trip.'
    default:
      return error.message
  }
}

function TripDetailSkeleton() {
  return (
    <LoadingRegion label="Loading trip">
      <div className="mx-auto max-w-6xl px-4 py-6">
        <Skeleton className="h-4 w-24" />
        <Skeleton className="mt-4 h-5 w-40" />
        <Skeleton className="mt-3 h-8 w-2/3" />
        <Skeleton className="mt-2 h-4 w-1/3" />
        <div className="mt-6 grid gap-6 lg:grid-cols-[1fr,20rem]">
          <div className="space-y-6">
            <div className="rounded-xl border border-slate-200 bg-white p-5">
              <SkeletonText lines={4} />
            </div>
            <Skeleton className="h-80 w-full rounded-xl" />
            <div className="rounded-xl border border-slate-200 bg-white p-5">
              <SkeletonText lines={6} />
            </div>
          </div>
          <div className="space-y-6">
            <Skeleton className="h-28 w-full rounded-xl" />
            <Skeleton className="h-56 w-full rounded-xl" />
          </div>
        </div>
      </div>
    </LoadingRegion>
  )
}
