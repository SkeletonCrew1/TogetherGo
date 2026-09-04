import type { SearchFilter } from '@/lib/types'

/**
 * Every cache key in one place.
 *
 * The reason it is one file rather than inline strings: invalidation is the
 * half of react-query that goes wrong, and it goes wrong when the key a
 * mutation invalidates is spelled a character differently from the key the
 * query used. Prefix arrays let a mutation invalidate a whole family —
 * `['trips']` covers every search page and every detail.
 */
export const keys = {
  me: ['me'] as const,

  users: {
    all: ['users'] as const,
    detail: (userId: string) => ['users', userId] as const,
  },

  trips: {
    all: ['trips'] as const,
    search: (filter: SearchFilter) => ['trips', 'search', filter] as const,
    detail: (tripId: string) => ['trips', 'detail', tripId] as const,
    similar: (tripId: string) => ['trips', 'similar', tripId] as const,
  },

  requests: {
    all: ['requests'] as const,
    forTrip: (tripId: string) => ['requests', 'trip', tripId] as const,
    mine: (tripId: string) => ['requests', 'mine', tripId] as const,
  },

  myTrips: {
    all: ['my-trips'] as const,
    byRole: (role: 'organizer' | 'participant') => ['my-trips', role] as const,
  },

  chat: {
    all: ['chat'] as const,
    rooms: ['chat', 'rooms'] as const,
    messages: (tripId: string) => ['chat', 'messages', tripId] as const,
  },

  ratings: {
    all: ['ratings'] as const,
    pending: ['ratings', 'pending'] as const,
  },
}
