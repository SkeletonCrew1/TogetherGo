import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'

import { Alert } from '@/components/ui/Alert'
import { Avatar } from '@/components/ui/Avatar'
import { Button } from '@/components/ui/Button'
import { EmptyState, ServiceUnavailableState } from '@/components/ui/EmptyState'
import { Textarea } from '@/components/ui/Field'
import { LoadingRegion, Skeleton } from '@/components/ui/Skeleton'
import { StarInput } from '@/components/ui/Stars'
import { keys } from '@/hooks/queryKeys'
import * as ratingsApi from '@/lib/api/ratings'
import { ApiError, errorMessage } from '@/lib/api/errors'
import { formatDateTime } from '@/lib/format'
import type { PendingRating, PublicProfile } from '@/lib/types'

export function Ratings() {
  const query = useQuery({
    queryKey: keys.ratings.pending,
    queryFn: () => ratingsApi.pending(),
    retry: false,
  })

  const notDeployed = query.error instanceof ApiError && query.error.status === 404

  return (
    <div className="mx-auto max-w-3xl px-4 py-6">
      <h1 className="text-2xl font-semibold text-slate-900">Rate your co-travellers</h1>
      <p className="mt-1 text-sm text-slate-600">
        A trip opens for rating once it completes, and closes again after the window.
      </p>

      <div className="mt-6">
        {query.isLoading ? (
          <LoadingRegion label="Loading pending ratings">
            <div className="space-y-4">
              <Skeleton className="h-40 w-full rounded-xl" />
              <Skeleton className="h-40 w-full rounded-xl" />
            </div>
          </LoadingRegion>
        ) : notDeployed ? (
          <ServiceUnavailableState
            service="ratings"
            what={
              'GET /api/ratings/pending answers 404. The gateway routes /api/ratings/* to identity, ' +
              'which stores rating_avg and rating_count on the user but has no ratings router yet — ' +
              'and the window is opened by trip.completed, which needs the completion transition too.'
            }
            reference="deploy/traefik/dynamic.yml"
          />
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
        ) : (query.data?.items.length ?? 0) === 0 ? (
          <EmptyState
            title="Nothing to rate right now"
            description="After a trip you were on completes, everyone who was there appears here."
            action={
              <Link to="/my-trips">
                <Button variant="secondary">See your trips</Button>
              </Link>
            }
          />
        ) : (
          <div className="space-y-6">
            {query.data?.items.map((entry) => <TripRatings key={entry.trip_id} entry={entry} />)}
          </div>
        )}
      </div>
    </div>
  )
}

function TripRatings({ entry }: { entry: PendingRating }) {
  return (
    <section className="rounded-xl border border-slate-200 bg-white">
      <header className="border-b border-slate-100 px-5 py-4">
        <h2 className="font-semibold text-slate-900">
          <Link to={`/trip/${entry.trip_id}`} className="hover:underline">
            {entry.trip_title}
          </Link>
        </h2>
        <p className="mt-0.5 text-sm text-slate-500">
          Rating closes {formatDateTime(entry.window_closes_at)}
        </p>
      </header>

      <ul className="divide-y divide-slate-100">
        {entry.ratees.map((ratee) => (
          <li key={ratee.id} className="px-5 py-4">
            <RateOne tripId={entry.trip_id} ratee={ratee} />
          </li>
        ))}
      </ul>
    </section>
  )
}

/**
 * One co-traveller, one rating.
 *
 * Submitted per person rather than as a batch for the whole trip: a rating is
 * about one relationship, a partial answer is a legitimate answer, and a batch
 * that fails halfway leaves the caller with no idea which of the six went
 * through.
 */
function RateOne({ tripId, ratee }: { tripId: string; ratee: PublicProfile }) {
  const queryClient = useQueryClient()
  const [score, setScore] = useState<number | null>(null)
  const [comment, setComment] = useState('')

  const mutation = useMutation({
    mutationFn: () =>
      ratingsApi.submit({
        trip_id: tripId,
        ratee_id: ratee.id,
        score: score as number,
        comment: comment.trim() ? comment.trim() : null,
      }),
    onSuccess: async () => {
      await Promise.all([
        // The person's own average has changed, and so has the list of who is
        // still owed a rating.
        queryClient.invalidateQueries({ queryKey: keys.ratings.pending }),
        queryClient.invalidateQueries({ queryKey: keys.users.detail(ratee.id) }),
        // identity republishes user.rating_updated, which trip projects into its
        // listings — so cards showing this person are stale too.
        queryClient.invalidateQueries({ queryKey: keys.trips.all }),
      ])
    },
  })

  if (mutation.isSuccess) {
    return (
      <div className="flex items-center gap-3">
        <Avatar name={ratee.full_name} photoUrl={ratee.photo_url} size="sm" />
        <p className="text-sm text-slate-600">
          <span className="font-medium text-slate-900">{ratee.full_name}</span> rated.
        </p>
      </div>
    )
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-3">
        <Avatar name={ratee.full_name} photoUrl={ratee.photo_url} size="sm" />
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm font-medium text-slate-900">{ratee.full_name}</p>
          {ratee.bio && <p className="truncate text-xs text-slate-500">{ratee.bio}</p>}
        </div>
        <StarInput
          value={score}
          onChange={setScore}
          name={`rating-${tripId}-${ratee.id}`}
          disabled={mutation.isPending}
        />
      </div>

      {score !== null && (
        <>
          <Textarea
            label={`A note about ${ratee.full_name}`}
            rows={2}
            maxLength={1000}
            value={comment}
            onChange={(event) => setComment(event.target.value)}
            placeholder="Optional."
          />
          <div className="flex items-center gap-2">
            <Button size="sm" isLoading={mutation.isPending} onClick={() => mutation.mutate()}>
              Submit rating
            </Button>
            <Button
              size="sm"
              variant="ghost"
              disabled={mutation.isPending}
              onClick={() => {
                setScore(null)
                setComment('')
              }}
            >
              Clear
            </Button>
          </div>
        </>
      )}

      {mutation.error && <Alert>{errorMessage(mutation.error)}</Alert>}
    </div>
  )
}
