import { api } from './client'
import type { JoinRequest, Page, Trip } from '@/lib/types'

export const requestToJoin = (tripId: string, message?: string | null) =>
  api.post<JoinRequest>(`/api/trips/${tripId}/requests`, { message: message || null })

/**
 * The caller's own request on this trip. 404 means they have never applied,
 * which is a normal answer and not an error — see useMyJoinRequest.
 */
export const myRequest = (tripId: string) =>
  api.get<JoinRequest>(`/api/trips/${tripId}/requests/me`)

export const cancelMyRequest = (tripId: string) =>
  api.delete<void>(`/api/trips/${tripId}/requests/me`)

/** The organizer's queue. Keyset paged, one identity call per page. */
export const listRequests = (tripId: string, cursor?: string | null) =>
  api.get<Page<JoinRequest>>(`/api/trips/${tripId}/requests`, {
    query: { cursor: cursor ?? undefined },
  })

export const approve = (tripId: string, requestId: string) =>
  api.post<JoinRequest>(`/api/trips/${tripId}/requests/${requestId}/approve`)

export const reject = (tripId: string, requestId: string, reason?: string | null) =>
  api.post<JoinRequest>(`/api/trips/${tripId}/requests/${requestId}/reject`, {
    reason: reason || null,
  })

/** Give up a seat. The organizer cannot: they cancel the trip instead. */
export const leave = (tripId: string) =>
  api.post<Trip>(`/api/trips/${tripId}/participants/me`)

export const removeParticipant = (tripId: string, userId: string) =>
  api.delete<void>(`/api/trips/${tripId}/participants/${userId}`)
