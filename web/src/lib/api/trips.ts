import { api } from './client'
import type {
  CreateTripBody,
  Page,
  SearchFilter,
  Trip,
  TripListItem,
  UpdateTripBody,
} from '@/lib/types'

/**
 * Discovery. Only recruiting trips that have not started yet are ever returned
 * — the scope is fixed in the service (store/search.go) and is not a filter the
 * SPA can widen, which is why /my-trips cannot be built on this endpoint.
 */
export const search = (filter: SearchFilter, cursor?: string | null) =>
  api.get<Page<TripListItem>>('/api/trips', {
    query: { ...filter, cursor: cursor ?? undefined },
  })

export const get = (tripId: string) => api.get<Trip>(`/api/trips/${tripId}`)

/** At most five, and no cursor: there is deliberately no way to ask for more. */
export const similar = (tripId: string) =>
  api.get<{ items: TripListItem[] }>(`/api/trips/${tripId}/similar`)

export const create = (body: CreateTripBody) => api.post<Trip>('/api/trips', body)

export const update = (tripId: string, body: UpdateTripBody) =>
  api.patch<Trip>(`/api/trips/${tripId}`, body)

export const remove = (tripId: string) => api.delete<void>(`/api/trips/${tripId}`)

/** draft -> recruiting. The trip becomes findable. */
export const publish = (tripId: string) => api.post<Trip>(`/api/trips/${tripId}/publish`)

export const cancel = (tripId: string) => api.post<Trip>(`/api/trips/${tripId}/cancel`)

/* --------------------------------------------------------------------------
 * Transitions with no route yet.
 *
 * The state machine (internal/domain/status.go) has the edges —
 * recruiting -> in_progress -> completed — and Store.ChangeStatus can make
 * them, but internal/http/router.go routes only /publish and /cancel. So
 * "force completion", which the acceptance criteria need in order to open the
 * rating window, has no endpoint to call.
 *
 * These follow the naming the two existing transitions already use.
 * ------------------------------------------------------------------------ */

/** recruiting -> in_progress. NOT BUILT — see web/README.md. */
export const start = (tripId: string) => api.post<Trip>(`/api/trips/${tripId}/start`)

/**
 * in_progress -> completed. NOT BUILT.
 *
 * This is the transition that publishes `trip.completed`, which identity
 * consumes to open mutual rating between roster members
 * (contracts/events.md), so nothing on /ratings can happen until it exists.
 */
export const complete = (tripId: string) => api.post<Trip>(`/api/trips/${tripId}/complete`)
