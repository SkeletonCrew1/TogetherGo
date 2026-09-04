# chat

The group conversation a trip gets once people are on it. This is the only
service in TogetherGo allowed to use WebSockets, and the only one that runs more
than one replica in the default stack — which is the same fact twice, and the
reason most of what follows is about how two processes serve one room.

It owns exactly one thing: messages. Rooms and membership are a **projection**
of `chat.trip-events`, and this service never calls the trip service to ask
about either.

## Endpoints

| Method | Path | |
| --- | --- | --- |
| `POST` | `/api/chat/tickets` | `{trip_id}` → a single-use, 30-second websocket credential |
| `GET` | `/api/chat/rooms` | the caller's rooms, most recently active first, with last message and unread count |
| `GET` | `/api/chat/rooms/{trip_id}/messages` | history, newest first, `?before_id=` or `?cursor=`, `?limit=` |
| `GET` | `/ws/chat/{trip_id}?ticket=…` | the socket |
| `GET` | `/healthz` | liveness, checks nothing |
| `GET` | `/readyz` | readiness: Postgres, identity's JWKS, Redis and the broker |

The three `/api/chat` routes take a normal `Authorization: Bearer` token, which
this service verifies itself against identity's JWKS (CLAUDE.md rule 6). The
socket does not — see below.

Client traffic reaches the service through the gateway on
`http://localhost:8080`. Ports 8003 and up are exposed in Compose for debugging
only; they are a *range* rather than a single port so that
`docker compose up --scale chat=2` does not fail on a bind conflict.

## Why there is a ticket endpoint

The browser `WebSocket` constructor takes a URL and nothing else. There is no
way to put an `Authorization` header on a handshake, and the two usual
workarounds are both bad here:

- **A cookie.** This system does not use them, and adding one would bring CSRF
  with it for the sake of one endpoint.
- **The access token in the query string.** A fifteen-minute credential, good
  against every service, written into Traefik's access log, the browser's
  history and the `Referer` of anything the page loads next.

So the SPA exchanges its token for a ticket:

```
POST /api/chat/tickets  {"trip_id": "..."}      → {"ticket": "...", "expires_in": 30}
GET  /ws/chat/{trip_id}?ticket=...
```

The ticket is 32 random bytes, base64url'd. It is stored in Redis under
`chat:ticket:<value>` with the user id and trip id as its *value*, so there is
nothing in the string itself to read or tamper with, and it expires in thirty
seconds. Leaking one costs an attacker a socket on a room they would have to be
a member of already.

**The handshake has three gates, in this order:**

1. The trip id parses. Cheapest, and a malformed one cannot be a real ticket's
   trip either.
2. The ticket redeems with `GETDEL` — one command, so two handshakes racing on
   the same value cannot both succeed — and the trip inside it is compared
   against the one in the URL. Membership of one room is not membership of all
   of them.
3. **Membership is re-read from the database**, not taken from the ticket. The
   ticket says who the caller is; the database says what they may do. Thirty
   seconds is short but it is not zero, and an organizer can remove somebody
   inside it.

### Why a refusal is a close code and not a status code

A browser cannot read the HTTP status of a failed websocket handshake:
`onclose` reports 1006 and the page cannot tell an expired ticket from a dropped
network. So every refusal completes the upgrade and immediately closes with a
code the client can branch on. RFC 6455 reserves 4000–4999 for exactly this.

| Code | Meaning | What the client should do |
| --- | --- | --- |
| `4400` | the trip id is not a uuid | fix the bug |
| `4401` | ticket missing, expired, spent, or for another trip | fetch a new ticket and retry |
| `4403` | not a member — or removed while connected | stop |
| `4404` | the room projection has not caught up | retry shortly |
| `1001` | the server is shutting down | reconnect |

## Frames

Client → server:

```json
{"type":"message","client_msg_id":"<uuid>","body":"..."}
{"type":"typing"}
{"type":"read","last_message_id":123}
```

Server → client:

```json
{"type":"message","id":123,"client_msg_id":"...","trip_id":"...","sender_id":"...","body":"...","created_at":"..."}
{"type":"presence","user_id":"...","status":"online"}
{"type":"typing","user_id":"..."}
{"type":"error","code":"rate_limited","message":"..."}
```

`client_msg_id` is echoed back so the sender can replace the optimistic row it
rendered before the round trip instead of showing the message twice. It goes to
every recipient rather than only the sender: it is a uuid the client generated,
it identifies nothing, and one pre-encoded frame written to every socket is what
keeps the fan-out to a single marshal per message.

`error` frames never close the connection. `error` codes use the same vocabulary
as the REST endpoints, so a client has one table rather than two:
`invalid_frame`, `unsupported_frame`, `validation_error`, `rate_limited`,
`room_closed`, `not_member`, `internal_error`.

A new socket is sent a `presence` frame per person already in the room, before
anything else. Otherwise a client would need a second endpoint to answer "who is
here", and it would be stale by the time it rendered.

## The fan-out, and why it exists

Every persisted message is published to the Redis channel
`chat:room:<trip_id>`. Each instance subscribes to the rooms it currently holds
a socket for and forwards to those sockets.

Without this, two users on two replicas cannot see each other's messages — and
crucially, **nothing looks broken with one replica**. The bug appears at the
exact moment you scale, which is why there is a test that runs two hubs against
one Redis (`TestMessagesReachSocketsOnAnotherReplica`).

Subscriptions are per room, refcounted by local socket count, rather than one
pattern subscription on `chat:room:*`. The pattern is a line shorter and
delivers every room's traffic to every replica.

Redis pub/sub is fire-and-forget: a message published while an instance is
disconnected is not replayed to it. That is acceptable *because the message is
already in Postgres before it is published* — the durable copy is the database,
this is only the live path, and a client that missed a frame refetches history
on reconnect.

The same channel carries two things that are not messages:

- a **typing** frame, with an `except` hint so it is not echoed to its author;
- an **evict**, which is how `participant.removed` closes a socket held by a
  replica other than the one that consumed the event. The consumer has no idea
  which instance that is, and asking would mean inventing service discovery for
  something Redis already does.

## Connection handling

One reader goroutine and one writer goroutine per connection, joined by a
buffered channel. The writer is the only thing that ever writes to the socket,
so there is no interleaved-frame bug to have.

- **A slow client is disconnected, not waited for.** The send is non-blocking:
  if the buffer is full, the client has failed to read `CHAT_SEND_BUFFER` frames
  while everyone else kept up, and the choice is between blocking the fan-out
  goroutine — which serves every room on the instance — and dropping one
  connection. It is not even lossy from the user's side: their client reconnects
  and refetches history, exactly as it does after any network blip.
- **Ping every 30s, close if no pong within 60.** A dropped TCP connection looks
  exactly like an idle one until something is written to it. The ping lives in
  the writer's `select` rather than a third goroutine, which is also what
  guarantees it never runs concurrently with a write.
- **Graceful shutdown closes every socket with 1001 Going Away**, not 1000. A
  client library treats 1001 as reconnect-worthy and 1000 as "we are done here";
  a rolling deploy depends on the difference.

`http.Server` here has no `ReadTimeout` and no `WriteTimeout`, unlike the trip
service's. Both are deadlines on the whole connection and net/http applies them
to a hijacked one too, so a 30-second `WriteTimeout` would kill every websocket
30 seconds after it opened. The protections they would give are provided
per-operation instead: `ReadHeaderTimeout` on the handshake, a 16 KiB read limit
and the ping/pong deadline on the socket, and a write deadline around each
frame.

## Rate limiting

Twenty messages per ten seconds, per user per room, in Redis.

Over the limit the sender gets an `error` frame and the message is dropped —
**the connection stays open**. Closing it would be the easy implementation and
the wrong one: a user typing fast is a user, and dropping their socket costs
them the conversation over something that corrects itself in ten seconds.

The counter is a Lua script — `INCR`, and `PEXPIRE` only on the increment that
created the key. Two round trips from Go would leave a window in which a crash
strands a counter with no expiry, silencing that user in that room until
somebody noticed. The window is anchored to the *first* message rather than to a
wall-clock bucket, because fixed buckets let a user send the limit twice across
a boundary — forty messages in a hundred milliseconds — which is the burst the
limit exists to stop.

It **fails open**: a Redis error allows the message and logs. A limiter that
failed closed would turn a Redis blip into a service that silently refuses
everything, which is a far worse outage than a few seconds of unlimited typing,
and Redis being down is already visible on `/readyz`.

## Presence

A Redis sorted set per room, `chat:presence:<trip_id>`, scored by each member's
last heartbeat. The heartbeat rides on the ping interval.

A sorted set rather than a plain set because a set has one expiry for the whole
key: a member left behind by a killed instance would stay online until the
entire room's presence expired and everyone appeared to leave at once. Per-member
scores mean a crashed instance's users drain out one TTL after their last
heartbeat while everybody else is untouched.

Announcements are driven by Redis's own return values rather than by anything a
process remembers. `ZADD` reports whether it *added* a member as opposed to
updating one, so the first heartbeat announces an arrival and the next thirty say
nothing; `ZREM` reports whether it removed one, so two replicas racing to drop
the same user announce a single departure. A user swept out by one instance
while still connected to another is re-added and re-announced on the next
heartbeat — presence is best-effort, and this is the shape of the error it
makes: one that corrects itself within a heartbeat rather than one that persists.

## The room projection

Rooms and membership come from four events and nothing else. There is no
endpoint that creates a room and none that adds a member.

| Event | |
| --- | --- |
| `trip.created` | create the room (`recruiting`) and seat the organizer |
| `join_request.approved` | add the participant — **the only thing that grants chat access** |
| `participant.removed` | set `left_at`, and close that user's sockets |
| `trip.cancelled` | close the room to new messages; history stays |

Every handler claims its `event_id` in `processed_events` inside the same
transaction as the write (CLAUDE.md rule 5), and every write is an upsert
underneath that. Belt and braces: the ledger absorbs a redelivery, and the upsert
would have made it harmless anyway.

**Events are unordered**, and this projection is written for it. A
`join_request.approved` can arrive before the `trip.created` that ought to have
preceded it — a busy organizer, two relay replicas, a plain redelivery. All three
room-touching payloads carry the trip's title, so each handler provisions the
room before touching membership, and a late `trip.created` becomes a title
refresh rather than a failure. The status is only ever written on insert, so a
cancellation that overtakes its own creation event is not undone by it.

If the projection has not caught up, **the connection is refused with 4404 and
the client retries.** This service does not call the trip service to find out
whether a room ought to exist — that is the synchronous dependency the whole
event-driven arrangement exists to avoid — and the gap is one outbox poll plus
one broker hop.

## Data model

```
rooms         trip_id pk, title, status, created_at
room_members  (trip_id, user_id) pk, joined_at, left_at, last_read_message_id
messages      id bigserial pk, trip_id, sender_id, body, created_at
              index on (trip_id, id desc)
processed_events  event_id pk, processed_at
```

Three things worth pointing at:

- **`messages.id` is a bigserial** — the only identifier in the system that is
  not a uuid. A room is read backwards in pages of "everything before message
  N", and that ordering has to be total, monotonic and cheap to compare. A v4
  uuid is none of those, and ordering by `created_at` needs a tiebreaker a
  sequence gives for free. `before_id` and the socket's `last_message_id` are
  both this number.
- **`room_members` rows are never deleted.** `participant.removed` sets
  `left_at`. The messages that participant wrote stay in the room and stay
  attributed: removal revokes access going forward, it does not put holes in
  everybody else's conversation.
- **`last_read_message_id`** is the one column in that table the trip service
  knows nothing about. It is per-user local state, and it lives here because the
  key it needs is already the primary key.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `CHAT_DATABASE_URL` | — | required; `chat_db` as `chat_user` |
| `JWKS_URL` | `http://identity:8001/.well-known/jwks.json` | required |
| `REDIS_URL` | — | **required**, unlike in trip |
| `RABBITMQ_URL` | — | required |
| `CHAT_QUEUE` | `chat.trip-events` | named, never declared |
| `CHAT_TICKET_TTL` | `30` | seconds |
| `CHAT_RATE_LIMIT` / `CHAT_RATE_WINDOW` | `20` / `10` | messages per seconds |
| `CHAT_PING_INTERVAL` / `CHAT_PONG_TIMEOUT` | `30` / `60` | seconds; the second must exceed the first |
| `CHAT_PRESENCE_TTL` | `90` | seconds; must exceed the ping interval |
| `CHAT_SEND_BUFFER` | `64` | frames per connection |
| `CHAT_ALLOWED_ORIGINS` | `localhost:*,127.0.0.1:*` | websocket origin allowlist |
| `CHAT_PORT` / `CHAT_PORT_RANGE_END` | `8003` / `8013` | the published host range |

Redis is required here and optional in trip, and that difference is the point.
In trip it is a cache and losing it costs latency. Here it is the ticket store,
the rate limiter and the fan-out: an instance that cannot reach it can issue no
tickets, accept no sockets, and deliver nothing written on another replica. It
is alive, it looks healthy, and it is useless — so `/readyz` reports it and the
gateway takes the instance out.

## Running it

```sh
make up                 # brings the stack up
make migrate-chat       # never runs on startup (CLAUDE.md)
make test-chat          # starts postgres, redis and rabbitmq containers
make test-chat-race     # the socket and fan-out tests under -race
make scale-chat         # two replicas behind the gateway (make scale-chat N=3)
make psql-chat
```

**Migrate before the first event arrives.** Migrations are a separate step by
design, and the consumer starts as soon as the container does — so on a fresh
volume, any event already sitting on `chat.trip-events` is handled against a
database with no tables, fails four times, and is dead-lettered. That is the
contract working (a transient failure that outlives its retries is a human's
problem, and the log line names the missing relation), and replaying from
`chat.trip-events.dlq` afterwards is safe because `processed_events` makes a
replay a no-op for anything already applied. It is still a minute of confusion
that `make up && make migrate` in that order avoids entirely.

The suite runs against real backing services. That is not thoroughness for its
own sake: the three things this service is made of — single-use tickets,
cross-replica fan-out, and a Lua rate limiter — are all properties of Redis
commands, and a fake would only prove that the fake agrees with the code written
against it.
