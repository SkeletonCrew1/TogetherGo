# TogetherGo — web

The SPA. React 18 + TypeScript + Vite, TanStack Query for server state, React
Router, Tailwind, react-hook-form + zod for forms, react-leaflet over
OpenStreetMap tiles for maps. No component library — the handful of primitives
this needs are in `src/components/ui/`.

```bash
cp .env.example .env
npm install
npm run dev        # http://localhost:5173
npm test           # 21 tests
npm run build      # tsc -b && vite build
```

`npm run dev` proxies `/api`, `/.well-known` and `/ws` to the Traefik gateway on
`localhost:8080`, so the SPA is same-origin with the API in development. That is
not just convenience: an httpOnly refresh cookie is only usable same-origin, so
the dev setup matches the shape the auth design is heading for. Bring the
backend up first with `make up && make migrate` from the repository root.

## Layout

```
src/
├── lib/
│   ├── api/          one module per service + the fetch client
│   ├── ws/           the chat socket: ticket, backoff, reconnect
│   ├── types.ts      every wire shape, named as the service names it
│   ├── geo.ts        6-dp rounding, reverse geocoding, distance
│   └── format.ts     dates, labels, badge styles
├── auth/             the in-memory token session and the route guards
├── components/
│   ├── ui/           Button, Field, Modal, Tabs, Stars, Skeleton, …
│   ├── map/          MapBase, MapPicker, RouteMap, RouteEditor
│   └── trip/         TripCard, Timeline, JoinRequestQueue, transport icons
├── hooks/            query keys, infinite lists, roster resolution
└── routes/           one file (or folder) per screen
```

## Screens

| Route | State |
| --- | --- |
| `/login` | Works against identity. |
| `/registration` | Works. Mirrors identity's own age and password rules. |
| `/main` | Works. Search, filters, radius map picker, infinite cursor list. |
| `/trip/:id` | Works. Timeline, Leaflet route, roster, context action button. Still derives the caller's standing locally and calls `/requests/me`; `GET /api/trips/{id}` now returns a `viewer` block that would replace both. |
| `/organize-trip` | Works. Map point editor, draft + publish. |
| `/my-trips` | Works. Both tabs, inline approve/reject, keyset paging. |
| `/profile` | Works. Inline edit per field, password change, logout. |
| `/chat/:tripId` | Built; **needs the chat service**. |
| `/ratings` | Built; **needs `/api/ratings/*`**. |

Where a screen needs an endpoint that answers 404 today, it says so explicitly
rather than rendering "you have nothing here" — the two call for completely
different actions, and showing the wrong one sends whoever is testing looking
for a bug in the SPA.

## Endpoints the SPA needs that do not exist yet

Everything below was verified against the running Compose stack. Nothing here
can be worked around client-side.

| Endpoint | Answer today | Why the SPA cannot fake it |
| --- | --- | --- |
| `POST /api/trips/{id}/start` | 404 | `recruiting -> in_progress` is a legal edge in `internal/domain/status.go`, but `internal/http/router.go` routes only `/publish` and `/cancel`. |
| `POST /api/trips/{id}/complete` | 404 | Same. This is the transition that publishes `trip.completed`, which is what opens the rating window — so nothing on `/ratings` can happen until it exists. |
| `GET /api/ratings/pending`, `POST /api/ratings` | 404 | Routed to identity at the gateway; identity has no ratings router (`app/api/` is auth, health, jwks, internal, users). It already owns the aggregate — `rating_avg`, `rating_count` — but not the write side. |

Chat is no longer on that list: `services/chat` is built and serves
`GET /api/chat/rooms`, `GET /api/chat/rooms/{tripId}/messages`,
`POST /api/chat/tickets` and `WS /ws/chat/{tripId}`.

There is deliberately **no** `POST /api/chat/rooms/{tripId}/messages`. The
messages endpoint is history and answers `GET` only; a message is sent as a
frame on the socket the page already holds:

```json
{ "type": "message", "client_msg_id": "<uuid v4>", "body": "..." }
```

The service echoes the message back to the whole room — the sender included —
with `client_msg_id` carried through, and that echo is what replaces the
optimistic bubble. Matching is on `client_msg_id` and never on body text. A
message typed while the socket is down is queued in order and flushed when it
reopens; there is no HTTP fallback, because there is no endpoint that would
accept one. See `src/routes/chat/useChatRoom.ts`.

The shapes the SPA expects are declared and commented in `src/lib/types.ts`.
The chat ones are transcribed from what `services/chat` actually serves — a
message id is a bigint, not a uuid, and the socket's `message` frame is flat
rather than nested. The rest are derived from `CLAUDE.md`'s routing table, the
conventions the built endpoints already follow (keyset page, the same error
envelope), and the guarantees in `contracts/events.md` — a chat room exists from `trip.created`, membership comes
from `join_request.approved`, `participant.removed` revokes it, and
`trip.cancelled` closes the room to new messages while keeping history.

There is also no notification service, so no mail reaches MailHog for any of
this. That is not a gap the SPA can close from its side.

## Auth

The access token is held **in memory only** — never `localStorage`, never a
cookie the SPA can read. It is short-lived (900s) and losing it on reload costs
one refresh call.

The refresh token is the one place the implementation differs from the brief,
because identity differs from it. The brief specifies an httpOnly cookie set by
identity; identity returns the refresh token in the JSON body of
`/api/auth/register`, `/login` and `/refresh`, and nothing in the service sets a
cookie. So `src/lib/api/tokens.ts` has both transports behind
`VITE_REFRESH_TRANSPORT`:

- `body` (default) — matches identity today. The token is kept in
  `sessionStorage`: per-tab, gone when the tab closes, and not shared with a
  second tab that may already have rotated it, which matters because reuse of a
  rotated token revokes the whole chain.
- `cookie` — the target. `/api/auth/refresh` is called with an empty body and
  `credentials: 'include'`, and this module stores nothing.

Flip the env var the day identity ships the cookie; no other change is needed.

**One refresh per burst.** Six queries mounting on `/main` with an expired token
all get 401 in the same tick. They await one shared in-flight promise, so there
is exactly one call to `/api/auth/refresh`. Firing six would not merely waste
five requests — refresh tokens rotate on every use, so five of them would
present an already-rotated token and log the user out. A 401 is retried **once**
and no more: an endpoint that answers 401 for its own reasons must not become an
infinite loop with a network call in it. Both are covered in
`src/lib/api/client.test.ts`.

## Chat socket

`src/lib/ws/chatSocket.ts` is a plain class, not a hook, so React's
mount/unmount churn (StrictMode double-invokes effects in dev) cannot turn into
a reconnect storm.

- A fresh ticket is requested before **every** connect, including every
  reconnect. Tickets are single-use, so replaying the last one is a guaranteed
  401 and a room that never comes back.
- Backoff is 1s, 2s, 4s, 8s, 16s, then 30s, each with full jitter. Without the
  jitter everyone in a room reconnects in lockstep after a service restart, and
  the first thing the service sees on the way back up is the herd that may have
  taken it down.
- The attempt counter resets on `onopen`, not on connect — a service that
  accepts a socket and immediately drops it would otherwise defeat the backoff.
- Sending goes over **HTTP**, not the socket. A send on the socket has no status
  code and cannot be retried without knowing whether the socket was open when it
  left. The composer works while the connection is down; only the fan-out is
  delayed.
- Optimistic rows are keyed by `client_msg_id` and dropped when a real message
  carrying that id arrives, from the POST response or the socket, whichever is
  first. A failed send stays on screen and offers retry or discard rather than
  vanishing — a message that disappears gets retyped, and then the room has it
  twice.

Covered in `src/lib/ws/chatSocket.test.ts`.

## Maps

OpenStreetMap tiles, no API key. Every marker is a Leaflet `divIcon` built in
`src/components/map/leafletSetup.ts`, not the default marker image: that image is
referenced by a relative URL from inside the package, which Vite does not
rewrite, and the result is the classic "markers are invisible in production"
bug. Numbered pins come for free the same way.

The picker rounds to **6 decimal places** — ~11 cm, and the precision the brief
fixes. It matters beyond tidiness: a click gives ~13 digits of float noise, so
without rounding two clicks on the same pixel are two different points and the
route is never clean.

The reverse-geocoded name is a **seed for an editable text field**, never the
stored value. Nominatim rate-limits its public instance hard, answers in
whatever language it likes, and regularly names a trailhead after the nearest
B-road. A guess in a field the user can fix is useful; one they cannot fix is a
wrong name on someone's trip.

The route polyline is a straight join between consecutive stops, not a routed
path. The service stores stops, not roads, and drawing a plausible driving route
between two of them would invent a claim the organizer never made.

## Lists

Every list uses the cursor from the API and there are no page numbers anywhere —
keyset pagination is fixed in `CLAUDE.md` and there is no total count on the wire
to build page numbers from. `next_cursor: null` is mapped to `undefined` so
`hasNextPage` goes false. Each list has an IntersectionObserver sentinel **and** a
"Load more" button: the observer never fires for someone who reached the bottom
with Ctrl+End.

## A note on `/trip/:id`

`GET /api/trips/{id}` returns participants as `{user_id, role, joined_at}` and
nothing more — trip owns seats, identity owns people, and there is no join across
that boundary. The roster's names and ratings are fetched one profile at a time
from `GET /api/users/{id}`. That is acceptable here and only here: capacity is
capped at 50, the query cache dedupes across the several places on the screen
that want the same person, and identity's batch resolver (`GET /internal/users`)
is deliberately not routed through the gateway — it is for services, and reaching
it from a browser is exactly what that route's absence prevents.
