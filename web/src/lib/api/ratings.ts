import { api } from './client'
import type { PendingRating, SubmitRatingBody } from '@/lib/types'

/**
 * `/api/ratings/*` — routed to identity at the gateway, and identity has no
 * router for it (`app/api/` is auth, health, jwks, internal, users).
 *
 * identity already owns the aggregate: `rating_avg` and `rating_count` are
 * columns on the user and are already served on both profile shapes, and
 * migration 0002 is named "profile_and_ratings". What is missing is the write
 * side and the window, which `trip.completed` opens.
 */

/** What this caller still owes, grouped by trip. */
export const pending = () => api.get<{ items: PendingRating[] }>('/api/ratings/pending')

export const submit = (body: SubmitRatingBody) => api.post<void>('/api/ratings', body)
