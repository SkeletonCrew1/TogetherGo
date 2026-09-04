import { useState } from 'react'
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query'

import { Alert } from '@/components/ui/Alert'
import { Avatar } from '@/components/ui/Avatar'
import { Button } from '@/components/ui/Button'
import { Textarea } from '@/components/ui/Field'
import { Modal } from '@/components/ui/Modal'
import { Skeleton } from '@/components/ui/Skeleton'
import { RatingStars } from '@/components/ui/Stars'
import { keys } from '@/hooks/queryKeys'
import * as participationApi from '@/lib/api/participation'
import { ApiError, errorMessage } from '@/lib/api/errors'
import { formatRelative } from '@/lib/format'
import type { JoinRequest } from '@/lib/types'

/**
 * The organizer's pending applications for one trip, inline on /my-trips.
 *
 * Only pending ones are shown. The endpoint returns the whole queue including
 * decided requests, and a list where yesterday's rejections sit between today's
 * applications is a list nobody can work through.
 */
export function JoinRequestQueue({ tripId }: { tripId: string }) {
  const queryClient = useQueryClient()
  const [rejecting, setRejecting] = useState<JoinRequest | null>(null)
  const [reason, setReason] = useState('')
  const [error, setError] = useState<string | null>(null)

  const query = useInfiniteQuery({
    queryKey: keys.requests.forTrip(tripId),
    queryFn: ({ pageParam }) => participationApi.listRequests(tripId, pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: (lastPage) => lastPage.next_cursor ?? undefined,
  })

  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: keys.requests.forTrip(tripId) }),
      // Approving takes a seat, which changes the trip everywhere it is shown.
      queryClient.invalidateQueries({ queryKey: keys.trips.all }),
      queryClient.invalidateQueries({ queryKey: keys.myTrips.all }),
    ])
  }

  const approveMutation = useMutation({
    mutationFn: (request: JoinRequest) => participationApi.approve(tripId, request.id),
    onSuccess: refresh,
    onError: (cause) => setError(describe(cause)),
  })

  const rejectMutation = useMutation({
    mutationFn: (request: JoinRequest) =>
      participationApi.reject(tripId, request.id, reason),
    onSuccess: async () => {
      setRejecting(null)
      setReason('')
      await refresh()
    },
    onError: (cause) => setError(describe(cause)),
  })

  const pending =
    query.data?.pages
      .flatMap((page) => page.items)
      .filter((request) => request.status === 'pending') ?? []

  if (query.isLoading) {
    return (
      <div className="space-y-2">
        <Skeleton className="h-14 w-full rounded-lg" />
        <Skeleton className="h-14 w-full rounded-lg" />
      </div>
    )
  }

  if (query.error) {
    return <Alert>{errorMessage(query.error)}</Alert>
  }

  if (pending.length === 0) {
    return <p className="text-sm text-slate-500">No requests waiting.</p>
  }

  return (
    <div className="space-y-3">
      {error && <Alert>{error}</Alert>}

      <ul className="space-y-2">
        {pending.map((request) => {
          const busy =
            (approveMutation.isPending && approveMutation.variables?.id === request.id) ||
            (rejectMutation.isPending && rejectMutation.variables?.id === request.id)

          return (
            <li
              key={request.id}
              className="rounded-lg border border-slate-200 bg-slate-50/60 p-3"
            >
              <div className="flex items-start gap-3">
                <Avatar
                  name={request.requester?.full_name ?? null}
                  photoUrl={request.requester?.photo_url}
                  size="sm"
                />
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-medium text-slate-900">
                    {request.requester?.full_name ?? 'Traveller'}
                  </p>
                  <RatingStars
                    value={request.requester?.rating_avg ?? null}
                    count={request.requester?.rating_count ?? null}
                    size="sm"
                  />
                  {request.message && (
                    <p className="mt-1.5 whitespace-pre-wrap text-sm text-slate-600">
                      {request.message}
                    </p>
                  )}
                  <p className="mt-1 text-xs text-slate-400">
                    asked {formatRelative(request.created_at)}
                  </p>
                </div>

                <div className="flex shrink-0 gap-2">
                  <Button
                    size="sm"
                    isLoading={busy && approveMutation.isPending}
                    onClick={() => approveMutation.mutate(request)}
                  >
                    Approve
                  </Button>
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={busy}
                    onClick={() => {
                      setRejecting(request)
                      setReason('')
                    }}
                  >
                    Reject
                  </Button>
                </div>
              </div>
            </li>
          )
        })}
      </ul>

      {query.hasNextPage && (
        <Button
          variant="ghost"
          size="sm"
          isLoading={query.isFetchingNextPage}
          onClick={() => void query.fetchNextPage()}
        >
          Load more requests
        </Button>
      )}

      <Modal
        open={rejecting !== null}
        onClose={() => setRejecting(null)}
        title={`Decline ${rejecting?.requester?.full_name ?? 'this request'}`}
        footer={
          <>
            <Button variant="secondary" onClick={() => setRejecting(null)}>
              Keep it open
            </Button>
            <Button
              variant="danger"
              isLoading={rejectMutation.isPending}
              onClick={() => rejecting && rejectMutation.mutate(rejecting)}
            >
              Decline
            </Button>
          </>
        }
      >
        <Textarea
          label="Reason"
          rows={3}
          maxLength={500}
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          placeholder="Optional, and only this person sees it."
          // The reason is served to the requester and the organizer and nobody
          // else — it is deliberately not on the bus, so no notification
          // template can quote it.
          hint="Shown to them on the trip page. Not included in any email."
        />
      </Modal>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError) {
    switch (error.code) {
      case 'trip_full':
        return 'Every seat is taken, so this request cannot be approved. Free a seat first.'
      case 'request_already_decided':
        return 'That request was already decided — reloading will show the current queue.'
      default:
        return error.message
    }
  }
  return errorMessage(error)
}
