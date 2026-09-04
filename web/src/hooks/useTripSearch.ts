import { useInfiniteQuery } from '@tanstack/react-query'

import * as tripsApi from '@/lib/api/trips'
import { keys } from './queryKeys'
import type { SearchFilter } from '@/lib/types'

/**
 * The infinite trip list behind /main.
 *
 * `next_cursor` is the only thing that decides whether there is another page —
 * there are no page numbers and no total count anywhere in this API, by design
 * (CLAUDE.md: keyset pagination, never OFFSET). `getNextPageParam` returning
 * undefined is what makes `hasNextPage` false, so the null the API sends on the
 * last page has to be mapped rather than passed through.
 */
export function useTripSearch(filter: SearchFilter) {
  return useInfiniteQuery({
    queryKey: keys.trips.search(filter),
    queryFn: ({ pageParam }) => tripsApi.search(filter, pageParam),
    initialPageParam: null as string | null,
    getNextPageParam: (lastPage) => lastPage.next_cursor ?? undefined,
    // A trip's free-slot count changes whenever anyone else is approved, so a
    // list fetched a minute ago is worth refetching on return to the screen.
    staleTime: 30_000,
  })
}
