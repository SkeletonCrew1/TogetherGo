import { useQueries } from '@tanstack/react-query'

import * as usersApi from '@/lib/api/users'
import { keys } from './queryKeys'
import type { PublicProfile } from '@/lib/types'

/**
 * Resolves a trip's roster to names and ratings.
 *
 * GET /api/trips/{id} returns participants as `{user_id, role, joined_at}` and
 * nothing else — trip owns seats, identity owns people, and there is no join
 * across that boundary (CLAUDE.md). The detail screen needs names and ratings,
 * so it fetches them one profile at a time from identity's public endpoint.
 *
 * One request per participant is acceptable here and only here: capacity is
 * capped at 50, the query cache dedupes across the several places on the screen
 * that want the same person, and identity's own batch resolver
 * (GET /internal/users) is deliberately not routed through the gateway — it is
 * for services, and reaching it from a browser is the thing that route's
 * absence is designed to prevent.
 */
export function useParticipantProfiles(userIds: string[]) {
  const unique = Array.from(new Set(userIds))

  const results = useQueries({
    queries: unique.map((userId) => ({
      queryKey: keys.users.detail(userId),
      queryFn: () => usersApi.publicProfile(userId),
      // Names and photos change rarely; five minutes keeps a roster of twelve
      // from refetching on every navigation back to the trip.
      staleTime: 5 * 60_000,
      retry: 1,
    })),
  })

  const profiles = new Map<string, PublicProfile>()
  results.forEach((result, index) => {
    if (result.data) profiles.set(unique[index], result.data)
  })

  return {
    profiles,
    /** True only while nothing has arrived yet — a partial roster still renders. */
    isLoading: results.length > 0 && results.every((result) => result.isLoading),
  }
}
