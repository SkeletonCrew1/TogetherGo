import { api } from './client'
import type { MyTripItem, Page } from '@/lib/types'

/**
 * `/api/my/*` — the dashboard, routed to the trip service at the gateway.
 *
 * It is a server-side query and cannot be worked around client-side. Discovery
 * is scoped in SQL to `status = 'recruiting' AND start_at > now()` and its
 * query-parameter allowlist is closed, so there is no way to ask
 * GET /api/trips for the caller's drafts, their finished trips, or the trips
 * they merely applied to.
 */
export const trips = (role: 'organizer' | 'participant', cursor?: string | null) =>
  api.get<Page<MyTripItem>>('/api/my/trips', {
    query: { role, cursor: cursor ?? undefined },
  })
