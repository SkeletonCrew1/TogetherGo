/**
 * The wire. Every type here is a shape some service actually serialises, and
 * the field names are its field names — no camelCase layer, because a mapping
 * layer is one more place a contract change can be absorbed silently instead
 * of failing the build.
 *
 * Where a type describes an endpoint that does not exist yet, it says so, and
 * the comment names the contract it was written from.
 */

// --- shared -----------------------------------------------------------------

/** The one error shape (CLAUDE.md). `details` is free-form per code. */
export interface ApiErrorBody {
  error: {
    code: string
    message: string
    details?: Record<string, unknown>
  }
}

/** Every list endpoint. `next_cursor` is null on the last page, never absent. */
export interface Page<T> {
  items: T[]
  next_cursor: string | null
}

export interface Coordinates {
  lat: number
  lng: number
}

/** A coordinate that carries a name — what the map picker produces. */
export interface Place extends Coordinates {
  name: string
}

// --- identity ---------------------------------------------------------------

export interface TokenPair {
  access_token: string
  refresh_token: string
  /** Seconds until access_token expires. 900 today. */
  expires_in: number
}

/** GET /api/users/me, and the `user` of an auth response. */
export interface PrivateProfile {
  id: string
  email: string
  full_name: string
  bio: string | null
  birth_date: string | null
  age: number | null
  phone: string | null
  photo_url: string | null
  rating_avg: number | null
  rating_count: number
  is_active: boolean
  created_at: string
  updated_at: string
}

/** GET /api/users/{id}. No email, no phone, no birth date — `age` instead. */
export interface PublicProfile {
  id: string
  full_name: string
  bio: string | null
  photo_url: string | null
  age: number | null
  rating_avg: number | null
  rating_count: number
  created_at: string
}

export interface AuthResponse extends TokenPair {
  user: PrivateProfile
}

export interface RegisterBody {
  email: string
  password: string
  full_name: string
  birth_date: string
  bio?: string | null
}

export interface LoginBody {
  email: string
  password: string
}

export interface UpdateProfileBody {
  full_name?: string
  bio?: string | null
  phone?: string | null
  photo_url?: string | null
}

// --- trip -------------------------------------------------------------------

export const CATEGORIES = ['nature', 'city', 'abroad', 'hiking', 'food', 'other'] as const
export type Category = (typeof CATEGORIES)[number]

export const TRIP_STATUSES = [
  'draft',
  'recruiting',
  'in_progress',
  'completed',
  'cancelled',
] as const
export type TripStatus = (typeof TRIP_STATUSES)[number]

/** The bounds the trip service validates against (internal/domain/input.go). */
export const TRIP_LIMITS = {
  titleMin: 3,
  titleMax: 120,
  descriptionMax: 4000,
  capacityMin: 2,
  capacityMax: 50,
  pointsMin: 2,
  pointsMax: 20,
  pointNameMax: 120,
  transportMax: 40,
  maxDurationDays: 60,
} as const

/** The bounds discovery validates against (internal/domain/search.go). */
export const SEARCH_LIMITS = {
  pageSize: 20,
  pageSizeMax: 50,
  radiusMinKm: 1,
  radiusMaxKm: 500,
  queryMax: 200,
} as const

export interface RoutePoint {
  id: string
  seq: number
  name: string
  lat: number
  lng: number
  /** null for a stop with no planned time. */
  arrive_at: string | null
  transport: string | null
}

export interface Participant {
  user_id: string
  role: 'organizer' | 'participant'
  joined_at: string
}

/** GET /api/trips/{id}, and the body of every trip mutation. */
export interface Trip {
  id: string
  organizer_id: string
  title: string
  description: string | null
  category: Category
  status: TripStatus
  capacity: number
  approved_count: number
  spots_left: number
  start_at: string
  end_at: string
  departure: Coordinates
  destination: Coordinates
  created_at: string
  updated_at: string
  points: RoutePoint[]
  /** Ids and roles only — no names. See useParticipantProfiles. */
  participants: Participant[]
  /**
   * The caller's own relationship to this trip. Present on
   * `GET /api/trips/{id}` and absent from the mutations that return a trip.
   * See TripViewer.
   */
  viewer?: TripViewer
}

/**
 * The organizer block on a list item. Every field but the id is nullable, and
 * that is the documented degraded mode: when trip cannot reach identity the
 * page still renders with an id and nulls.
 */
export interface OrganizerRef {
  id: string
  full_name: string | null
  photo_url: string | null
  rating_avg: number | null
}

export interface RouteSummary {
  points: string[]
  total_points: number
  /** True when `points` is a prefix of the route rather than all of it. */
  truncated: boolean
}

/** One search result. `free_slots` is the same number detail calls `spots_left`. */
export interface TripListItem {
  id: string
  title: string
  category: Category
  status: TripStatus
  start_at: string
  end_at: string
  capacity: number
  approved_count: number
  free_slots: number
  departure: Place
  destination: Place
  route_summary: RouteSummary
  organizer: OrganizerRef
}

export interface PointBody {
  name: string
  lat: number
  lng: number
  arrive_at?: string | null
  transport?: string | null
}

export interface CreateTripBody {
  title: string
  description?: string | null
  category: Category
  capacity: number
  start_at: string
  end_at: string
  points: PointBody[]
}

export type UpdateTripBody = Partial<CreateTripBody>

/**
 * GET /api/trips query parameters. The service rejects anything not in here.
 *
 * A type alias rather than an interface, deliberately: only an alias of an
 * object literal type gets TypeScript's implicit index signature, which is what
 * lets a filter be handed straight to `buildUrl` without a cast. As an
 * interface it could only be passed as a spread, and every call site would grow
 * an `as Record<string, ...>` that defeats the point of typing it.
 */
export type SearchFilter = {
  q?: string
  date_from?: string
  date_to?: string
  near_lat?: number
  near_lng?: number
  radius_km?: number
  dest_lat?: number
  dest_lng?: number
  dest_radius_km?: number
  min_days?: number
  max_days?: number
  min_free_slots?: number
  categories?: Category[]
  limit?: number
}

// --- participation ----------------------------------------------------------

export const JOIN_STATUSES = ['pending', 'approved', 'rejected', 'cancelled'] as const
export type JoinRequestStatus = (typeof JOIN_STATUSES)[number]

/** The applicant as the organizer's queue shows them. Nullable but for the id. */
export interface RequesterRef {
  id: string
  full_name: string | null
  photo_url: string | null
  rating_avg: number | null
  rating_count: number | null
}

export interface JoinRequest {
  id: string
  trip_id: string
  user_id: string
  status: JoinRequestStatus
  message: string | null
  /** The organizer's own words on a rejection. */
  reason: string | null
  created_at: string
  decided_at: string | null
  decided_by: string | null
  /** Absent on the requester's own view of their request — they know who they are. */
  requester?: RequesterRef
}

// --- my trips ---------------------------------------------------------------

/**
 * The dashboard, served by the trip service at `/api/my/*`.
 *
 *   GET /api/my/trips?role=organizer|participant&status=&cursor=&limit=
 *     -> Page<MyTripItem>
 *
 * The same card discovery returns, plus two fields the dashboard adds. Both are
 * optional on the wire — a search result omits them rather than sending null —
 * which is why `pending_requests_count` is optional here too.
 */
export interface MyTripItem extends TripListItem {
  /**
   * The caller's standing on this trip. `requested` is an application nobody
   * has answered yet; `approved` is a seat. Note it is not the join request's
   * own status: a trip you were turned down for is not one of your trips.
   */
  membership_status: 'organizer' | 'approved' | 'requested'
  /**
   * How many applications are waiting. Present on `role=organizer` rows only —
   * how many people are queueing for a trip is the organizer's business — and
   * `0` there rather than absent.
   */
  pending_requests_count?: number
}

/**
 * The caller's own relationship to one trip, on `GET /api/trips/{id}`.
 *
 * It is what the detail page chooses between "Request to join", "Requested",
 * "Chat" and "Leave" with. `is_participant` is true for the organizer too, so
 * read `is_organizer` first. `join_request_status` is only ever `pending`,
 * `rejected` or null: an approved request has become a seat, and a withdrawn
 * one leaves you where you started.
 */
export interface TripViewer {
  is_organizer: boolean
  is_participant: boolean
  join_request_status: 'pending' | 'rejected' | null
}

// --- chat -------------------------------------------------------------------

/**
 * The chat service's wire shapes, as `services/chat` actually serves them.
 *
 *   GET  /api/chat/rooms                           -> Page<ChatRoom>
 *   GET  /api/chat/rooms/{tripId}/messages?cursor= -> Page<ChatMessage>  (newest first)
 *   POST /api/chat/tickets                         -> ChatTicket   (single-use)
 *   WS   /ws/chat/{tripId}?ticket=...
 *
 * There is no POST for a message and there is not meant to be one: the room is
 * a WebSocket, and a client that has one open sends over it. `/messages` is
 * history and answers GET only.
 *
 * A message id is a bigint, not a uuid — it is the sequence the service pages
 * and orders by, and the cursor is a base64 of it.
 */
export interface ChatMessage {
  id: number
  trip_id: string
  sender_id: string
  body: string
  created_at: string
  /**
   * Echoed back on the socket's `message` frame, carrying whatever the sender
   * generated. It is what lets the sender replace its optimistic bubble instead
   * of rendering the message twice. Absent on history rows, which is why it is
   * optional here.
   */
  client_msg_id?: string
}

export interface ChatRoom {
  trip_id: string
  title: string
  /** The trip's lifecycle status, projected from the event bus. */
  status: string
  /** `status !== "cancelled"`: history stays readable, composing stops. */
  is_open: boolean
  /** null in a room where nobody has said anything yet. */
  last_message: ChatMessage | null
  unread_count: number
  created_at: string
}

/**
 * A single-use, short-lived ticket for the WebSocket handshake.
 *
 * `expires_in` in seconds rather than an absolute timestamp, because the only
 * use for it is "is this still worth spending", and a relative number cannot be
 * got wrong by a browser whose clock is off.
 */
export interface ChatTicket {
  ticket: string
  expires_in: number
}

/**
 * A frame on the socket. Dispatched on `type`; unknown types are dropped.
 *
 * The `message` frame is flat rather than nested — the service writes the
 * message's own fields alongside `type`, so that history rows and live frames
 * are the same shape and render through the same code.
 */
export type ChatFrame =
  | ({ type: 'message' } & ChatMessage)
  | { type: 'presence'; user_id: string; status: 'online' | 'offline' }
  | { type: 'typing'; user_id: string }
  | { type: 'error'; code: string; message: string }

/** What the composer writes to the socket. The service accepts nothing else. */
export interface OutboundMessageFrame {
  type: 'message'
  client_msg_id: string
  body: string
}

/**
 * Error frame codes, from services/chat/internal/hub/frames.go.
 *
 * Switched on rather than the message text, which is the same rule the REST
 * error envelope follows.
 */
export const CHAT_ERROR_RATE_LIMITED = 'rate_limited'

// --- ratings (not built yet) ------------------------------------------------

/**
 * `/api/ratings/*` is routed to identity and marked "not built yet" there.
 * identity already stores the aggregate (`rating_avg`, `rating_count` on the
 * user) and contracts/events.md fixes the rest: `trip.completed` is what opens
 * the window, it carries `rating_window_closes_at`, and identity rejects new
 * ratings for the trip after it.
 *
 *   GET  /api/ratings/pending  -> { items: PendingRating[] }
 *   POST /api/ratings          -> void
 */
export interface PendingRating {
  trip_id: string
  trip_title: string
  window_closes_at: string
  /** The co-travellers this caller has not rated yet. */
  ratees: PublicProfile[]
}

export interface SubmitRatingBody {
  trip_id: string
  ratee_id: string
  /** 1–5 whole stars. */
  score: number
  comment?: string | null
}
