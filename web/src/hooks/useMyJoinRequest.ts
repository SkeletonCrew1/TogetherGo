import { useQuery } from '@tanstack/react-query'

import * as participationApi from '@/lib/api/participation'
import { ApiError } from '@/lib/api/errors'
import { keys } from './queryKeys'
import type { JoinRequest } from '@/lib/types'

/**
 * The caller's own join request on a trip, or null if they have never applied.
 *
 * The 404 is caught rather than surfaced, because "you have not applied" is the
 * common case, not a failure: the action button on /trip/:id needs to
 * distinguish "no request" from "request pending" from "rejected", and only one
 * of those three is an error to anybody.
 *
 * `retry: false` for the same reason — retrying a 404 three times before
 * concluding the user has not applied delays the button by a second for
 * nothing.
 */
export function useMyJoinRequest(tripId: string, enabled = true) {
  return useQuery<JoinRequest | null>({
    queryKey: keys.requests.mine(tripId),
    enabled,
    retry: false,
    queryFn: async () => {
      try {
        return await participationApi.myRequest(tripId)
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) return null
        throw error
      }
    },
  })
}
