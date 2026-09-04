import { useInfiniteQuery } from '@tanstack/react-query'

import * as myApi from '@/lib/api/my'
import { keys } from './queryKeys'

/**
 * Both tabs of /my-trips, one role at a time.
 *
 * The role is part of the query key, so switching tabs is a separate cached
 * infinite query rather than a refetch of the same one — the two tabs return
 * different sets and paginate independently.
 */
export function useMyTrips(role: 'organizer' | 'participant') {
  return useInfiniteQuery({
    queryKey: keys.myTrips.byRole(role),
    queryFn: ({ pageParam }) => myApi.trips(role, pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: (lastPage) => lastPage.next_cursor ?? undefined,
  })
}
