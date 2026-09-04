# TogetherGo event catalogue

This is the canonical contract for every message on the bus. A service may
publish only the events listed here as its own, and may consume only the
events listed here. Changing a payload in a backwards-incompatible way means
publishing a new `event_version`, not editing the shape in place.

## Transport

| Property | Value |
| --- | --- |
| Broker | RabbitMQ 3.13 |
| Exchange | `togethergo.events` |
| Exchange type | `topic`, durable |
| Routing key | the `event_type` value, verbatim |
| Delivery mode | persistent (2) |
| Content type | `application/json`, UTF-8 |
| Dead-letter exchange | `togethergo.dlx`, one DLQ per consumer queue |

Nothing in this table is declared by application code. The broker imports the
whole topology at boot from `deploy/rabbitmq/definitions.json`, so exchanges,
queues and bindings exist before the first service connects. A publisher does
`basic.publish` to `togethergo.events`; a consumer does `basic.consume` on its
queue. **Neither declares anything** — no `exchange_declare`, no
`queue_declare`, no `queue_bind`. A service that declares its own topology will
sooner or later declare it with different arguments than the file, and the
broker will answer with `PRECONDITION_FAILED`.

## Queues and bindings

One queue per consuming service per source of events, not one queue per event
type. A service reads its whole inbox from a single consumer and dispatches on
`event_type` internally.

| Queue | Consumer | Bound routing keys |
| --- | --- | --- |
| `notification.events` | notification | `user.registered`, `trip.status_changed`, `trip.completed`, `trip.cancelled`, `trip.invite_sent`, `join_request.*` |
| `chat.trip-events` | chat | `trip.created`, `trip.cancelled`, `join_request.approved`, `participant.removed` |
| `identity.trip-events` | identity | `trip.completed` |
| `trip.user-events` | trip | `user.profile_updated`, `user.rating_updated` |

`join_request.*` is the only wildcard binding, and it is deliberate:
notification wants every join-request lifecycle event, including any added
later. Every other binding names an exact routing key, so adding an event type
means adding a binding — an explicit decision about who receives it.

Each of those queues is durable and carries:

```
x-dead-letter-exchange:    togethergo.dlx
x-dead-letter-routing-key: <the queue's own name>
```

and has a matching `<queue>.dlq` — `notification.events.dlq`,
`chat.trip-events.dlq`, `identity.trip-events.dlq`, `trip.user-events.dlq` —
bound to `togethergo.dlx` with that same routing key.

Overriding the dead-letter routing key matters. Without it a dead letter keeps
its original routing key (`trip.cancelled`, say) and would land in every DLQ
whose queue happens to consume that event, telling you nothing about where the
failure actually was. With it, a message that failed on `chat.trip-events` goes
to `chat.trip-events.dlq` and nowhere else. The original routing key is still
readable in the `x-death` header RabbitMQ adds.

DLQs carry no dead-letter arguments of their own. A dead letter is a terminus;
re-dead-lettering it would be a loop.

Consumers must handle an `event_type` they do not recognise — a wildcard
binding or a redeployment order can deliver one. The rule is: log it at `warn`,
mark it processed, ack. An unknown event is not an error; it is a message meant
for a newer version of the service.

## Envelope

Every message body is a JSON object with exactly these five envelope fields
plus `payload`:

```json
{
  "event_id": "3f1c7a9e-6b2d-4f0a-9c11-8ad4e2b7c501",
  "event_type": "join_request.approved",
  "event_version": 1,
  "occurred_at": "2026-08-27T12:00:00Z",
  "aggregate_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
  "payload": {}
}
```

| Field | Type | Notes |
| --- | --- | --- |
| `event_id` | uuid v4 | Unique per publish. The idempotency key for consumers. |
| `event_type` | string | Dotted name; equals the routing key. |
| `event_version` | integer | Starts at 1. Bumped only on a breaking payload change. |
| `occurred_at` | RFC 3339, UTC | When the domain fact happened, not when it was published. |
| `aggregate_id` | uuid v4 | The trip or user the event concerns. Used for ordering and tracing. |
| `payload` | object | Event-specific; documented per event below. |

## Delivery semantics

**Every event is delivered at least once.** The relay publishes, then marks the
outbox row published; a crash between the two republishes the same event. On
the other end, a consumer that commits its transaction and dies before the ack
gets the message redelivered. Neither is a bug to be fixed — it is the
contract, and the consumer side absorbs it.

There is no ordering guarantee across events. Multiple relay replicas and
multiple consumer processes both break it. Events whose order matters carry a
timestamp (`updated_at`, `changed_at`) and the consumer discards anything older
than what it has already applied.

**Idempotency.** Every consumer keeps, in its own database:

```sql
CREATE TABLE processed_events (
    event_id     uuid PRIMARY KEY,
    processed_at timestamptz NOT NULL DEFAULT now()
);
```

The handler runs one transaction:

1. `INSERT INTO processed_events (event_id) VALUES ($1) ON CONFLICT DO NOTHING`
2. if it inserted no row, the event was already handled — commit, ack, stop
3. apply the side effects
4. commit

The insert comes first so it takes the row lock before any work happens: two
workers handed the same `event_id` concurrently serialise on it, and the loser
finds the row on its retry. Business writes stay idempotent on their own too
(upsert room membership rather than insert) — `processed_events` is the belt,
the upsert is the braces.

Side effects that leave the database — sending mail, mostly — cannot be rolled
back with the transaction. Do them **after** the commit, before the ack. A
crash in that window redelivers the event, `processed_events` reports it as
already handled, and the mail is not sent twice; the cost is that a message can
be lost in the rare crash between commit and send. For a welcome email that is
the right trade. Never send before committing.

**Acknowledgement.** Manual ack only — `autoAck: false` in Go,
`auto_ack=False` in Python. The order is always:

```
receive → handler transaction commits → basic.ack
```

Acking before the commit means a crash silently drops the event. Acking a batch
is not allowed either: one ack per message, no `multiple: true`.

Consumers set a prefetch of **10** (`basic.qos(prefetch_count=10)` /
`ch.Qos(10, 0, false)`). Unacked messages beyond that stay on the broker, so a
restart redelivers them and a slow consumer does not accumulate an unbounded
in-memory backlog.

**Failure handling.** Two kinds of failure, handled differently:

| Kind | Examples | Action |
| --- | --- | --- |
| Transient | database unavailable, deadlock, SMTP timeout, broker hiccup | retry in-process, then DLQ |
| Unrecoverable | body is not JSON, envelope fields missing or malformed, payload fails validation, referenced aggregate cannot exist | no retry, straight to the DLQ |

Unrecoverable means retrying cannot change the outcome. Nack it immediately:
`basic.nack(requeue=false)` (`d.Nack(false, false)` in Go,
`channel.basic_nack(delivery_tag, requeue=False)` in Python). It is dead-
lettered to `togethergo.dlx` and lands in `<queue>.dlq`.

Transient failures get **3 retries inside the handler**, with exponential
backoff and jitter:

| Attempt | Delay before it |
| --- | --- |
| 1 | — |
| 2 | 200 ms ± jitter |
| 3 | 400 ms ± jitter |
| 4 | 800 ms ± jitter |

Jitter is uniform in ±20 %, so replicas that failed together do not retry in
lockstep. If the fourth attempt still fails, `basic.nack(requeue=false)` — the
message goes to the DLQ and the consumer logs at `error` with `event_id`,
`event_type` and the final error.

**Never `requeue=true`.** A requeue puts the message back at the head of the
same queue, it is redelivered immediately, it fails again, and the consumer
spins at full speed on a poison message. Retry in-process where the backoff is
under our control, then dead-letter.

Total time in the handler is therefore bounded by roughly 1.4 s of backoff plus
the work itself, which is short enough to hold a prefetched delivery without
tripping any broker timeout.

**The DLQ is a human queue.** Nothing consumes it automatically, nothing
retries from it on a timer. It is an alert: a message there means a bug or an
outage that outlived the retries. The fix is to work out why, deploy the fix,
and replay by shovelling messages back into `togethergo.events` — safe to do,
because `processed_events` makes replay a no-op for anything already handled.

## Outbox contract

Every publisher writes events to an `outbox` table in its own database, in the
same transaction as the domain change. A relay inside the same service polls
that table and publishes. This is what makes "the trip was created" and "the
`trip.created` event exists" one atomic fact instead of two.

Both Go and Python services use the same table:

```sql
CREATE TABLE outbox (
    id           uuid        PRIMARY KEY,
    aggregate_id uuid        NOT NULL,
    event_type   text        NOT NULL,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);

-- The relay only ever reads unpublished rows. A partial index keeps that scan
-- proportional to the backlog rather than to the table.
CREATE INDEX outbox_unpublished_idx
    ON outbox (created_at)
    WHERE published_at IS NULL;
```

| Column | Notes |
| --- | --- |
| `id` | Generated by the writer, not the database. **It becomes `event_id` on the wire** — which is what makes the whole chain idempotent: a row republished after a relay crash carries the same `event_id`, and consumers deduplicate on it. |
| `aggregate_id` | The trip or user the event concerns; copied to the envelope. |
| `event_type` | Routing key. |
| `payload` | The envelope's `payload` object only — never the whole envelope. The relay assembles the envelope around it. |
| `created_at` | Becomes `occurred_at`. The domain fact happened when the transaction wrote this row, not when the relay got around to it. |
| `published_at` | `NULL` until the broker confirms the publish. The only mutable column. |

`event_version` is not stored. The relay stamps the current version for each
`event_type` from a constant in code — the version is a property of the
publishing build, and holding it in one place keeps a version bump from
requiring a data migration of rows already written.

### The relay

A goroutine or asyncio task inside the publishing service, not a separate
deployable. It polls in batches of **100**:

```sql
SELECT id, aggregate_id, event_type, payload, created_at
FROM outbox
WHERE published_at IS NULL
ORDER BY created_at
LIMIT 100
FOR UPDATE SKIP LOCKED;
```

`FOR UPDATE SKIP LOCKED` is the part that makes replicas safe. Each relay locks
the rows it selected for the life of its transaction; a second relay running the
same query at the same moment skips those locked rows and picks up the next
hundred instead of blocking on them or double-publishing them. Scaling the
service to three replicas needs no leader election and no coordination.

Per batch, in one transaction:

1. `SELECT ... FOR UPDATE SKIP LOCKED` as above.
2. Publish each row to `togethergo.events` — routing key `event_type`, body the
   assembled envelope, `delivery_mode: 2`, **publisher confirms on**.
3. Wait for the confirms.
4. `UPDATE outbox SET published_at = now() WHERE id = ANY($1)` for the confirmed
   ids.
5. Commit.

If the process dies at any point before step 5, the transaction rolls back, the
locks are released, the rows are still `published_at IS NULL`, and the next tick
publishes them again. That is precisely where at-least-once delivery comes from,
and why the `event_id` must be the stable `outbox.id` rather than something
generated at publish time.

A publish that is nacked or times out leaves that row unmarked; it is retried on
the next tick. The relay does not retry inside the tick and does not attempt to
preserve order across ticks.

**The poll interval is a per-service setting, not part of this contract.** It
is a latency/idle-query trade-off local to each publisher and nothing on the
other side of the bus can observe it: identity polls every second
(`OutboxRelay(poll_interval=1.0)`), trip every 500 ms (`TRIP_OUTBOX_POLL_MS`).
What *is* fixed here is the shape of the tick — one transaction, `SKIP LOCKED`,
publisher confirms, mark-then-commit — because that is what makes at-least-once
delivery true. A tick that finds a full batch polls again immediately rather
than idling, so a backlog drains at the speed of the broker in either service.

**A failed tick backs off.** A broker that is down would otherwise be a failed
connection attempt at the poll interval for as long as the outage lasts. The
delay doubles from the poll interval up to a ceiling — 30 seconds in trip — and
resets on the first success. Nothing is at risk while it does: the rows are
still `published_at IS NULL`, and "retry" and "the next tick" are the same
thing, which is why there is no per-row attempt counter anywhere.

Rows are published in `created_at` order within a batch, but `SKIP LOCKED` and
concurrent relays mean there is **no global ordering guarantee** — which is why
consumers use `updated_at` fields rather than arrival order to resolve conflicts.

### Cleanup

Published rows are kept for **7 days** — long enough to answer "did we actually
emit that event?" during an incident, short enough that the table stays small.
A periodic job in the same service deletes them (hourly in identity, daily in
trip; like the poll interval this is local policy, and the retention window is
the part that matters):

```sql
DELETE FROM outbox
WHERE id IN (
    SELECT id
    FROM outbox
    WHERE published_at IS NOT NULL
      AND published_at < now() - interval '7 days'
    LIMIT 10000
);
```

Bounded by `LIMIT` and repeated until it deletes fewer than 10 000 rows, so the
job never takes a long lock or blows out a single transaction. Unpublished rows
are never deleted by cleanup, however old: a row still `NULL` after seven days
is a stuck event and needs a human, not a `DELETE`.

## Catalogue

| Routing key | Publisher | Consumers | Outbox |
| --- | --- | --- | --- |
| `user.registered` | identity | notification | yes |
| `user.profile_updated` | identity | trip | yes |
| `user.rating_updated` | identity | trip | yes |
| `trip.created` | trip | chat | yes |
| `trip.status_changed` | trip | chat, notification | yes |
| `trip.completed` | trip | identity, notification | yes |
| `trip.cancelled` | trip | chat, notification | yes |
| `trip.invite_sent` | trip | notification | yes |
| `join_request.created` | trip | notification | yes |
| `join_request.approved` | trip | chat, notification | yes |
| `join_request.rejected` | trip | notification | yes |
| `participant.removed` | trip | chat | yes |

Note the direction of the two projections into `trip`: the trip service keeps a
read-only local copy of the display name, avatar and rating of users it cares
about, fed by `user.profile_updated` and `user.rating_updated`. That projection
is what makes "no SQL join across service boundaries" workable — trip never
queries `identity_db`.

---

### `user.registered`

A new account has been created. The email is not yet verified.

- **Routing key:** `user.registered`
- **Publisher:** identity
- **Consumers:** notification — sends the welcome / verification email
- **Aggregate:** the user
- **Outbox:** yes

```json
{
  "event_id": "0f8a1c2b-3d4e-4f50-8a61-7b2c9d0e1f23",
  "event_type": "user.registered",
  "event_version": 1,
  "occurred_at": "2026-08-27T12:00:00Z",
  "aggregate_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
  "payload": {
    "user_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "email": "traveller@example.com",
    "display_name": "Alex",
    "verification_token": "s7Kd0Qm2Yb1p...",
    "registered_at": "2026-08-27T12:00:00Z"
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `user_id` | uuid | yes | Same as `aggregate_id`. |
| `email` | string (email) | yes | Address to send verification to. |
| `display_name` | string | yes | 1–64 characters. |
| `verification_token` | string | yes | Single-use. The only credential-ish value on the bus; never log it. |
| `registered_at` | RFC 3339 | yes | Account creation time. |

---

### `user.profile_updated`

Public profile fields changed. Feeds the trip service's user projection.

- **Routing key:** `user.profile_updated`
- **Publisher:** identity
- **Consumers:** trip — updates its local projection of the user
- **Aggregate:** the user
- **Outbox:** yes

```json
{
  "event_id": "1a2b3c4d-5e6f-4071-8293-a4b5c6d7e8f9",
  "event_type": "user.profile_updated",
  "event_version": 1,
  "occurred_at": "2026-08-27T12:05:00Z",
  "aggregate_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
  "payload": {
    "user_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "display_name": "Alex K.",
    "avatar_url": "https://cdn.example.com/a/9c1e5b7a.jpg",
    "bio": "Weekend hiker, slow walker, good with maps.",
    "updated_at": "2026-08-27T12:05:00Z"
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `user_id` | uuid | yes | Same as `aggregate_id`. |
| `display_name` | string | yes | Current value, not a delta. |
| `avatar_url` | string \| null | yes | Null when the user has no avatar. |
| `bio` | string \| null | yes | Up to 500 characters. |
| `updated_at` | RFC 3339 | yes | Consumers use this to discard out-of-order updates. |

The payload always carries the full current profile. A consumer that receives
an `updated_at` older than the one it has stored discards the message (it is
still marked processed).

---

### `user.rating_updated`

A user's aggregate rating changed, because a new co-traveller rating was
recorded. Keeps trip listings able to show ratings without calling identity.

- **Routing key:** `user.rating_updated`
- **Publisher:** identity
- **Consumers:** trip — updates its local projection
- **Aggregate:** the rated user
- **Outbox:** yes

```json
{
  "event_id": "2b3c4d5e-6f70-4182-93a4-b5c6d7e8f901",
  "event_type": "user.rating_updated",
  "event_version": 1,
  "occurred_at": "2026-08-30T09:00:00Z",
  "aggregate_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
  "payload": {
    "user_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "rating_average": 4.75,
    "rating_count": 12,
    "updated_at": "2026-08-30T09:00:00Z"
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `user_id` | uuid | yes | The user who was rated. |
| `rating_average` | number | yes | 1.00–5.00, two decimal places. |
| `rating_count` | integer | yes | Total ratings received. |
| `updated_at` | RFC 3339 | yes | Same out-of-order rule as `user.profile_updated`. |

---

### `trip.created`

A trip has been published. Chat provisions the group conversation up front so
the organizer has somewhere to post before anyone joins.

- **Routing key:** `trip.created`
- **Publisher:** trip
- **Consumers:** chat — creates the room with the organizer as its first member
- **Aggregate:** the trip
- **Outbox:** yes

```json
{
  "event_id": "3c4d5e6f-7081-4293-a4b5-c6d7e8f90123",
  "event_type": "trip.created",
  "event_version": 1,
  "occurred_at": "2026-08-27T13:00:00Z",
  "aggregate_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
  "payload": {
    "trip_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
    "organizer_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "title": "Carpathians ridge, two days",
    "category": "hike",
    "starts_at": "2026-09-12T06:00:00Z",
    "ends_at": "2026-09-13T18:00:00Z",
    "max_participants": 8,
    "status": "open"
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `trip_id` | uuid | yes | Same as `aggregate_id`. |
| `organizer_id` | uuid | yes | Becomes the room owner in chat. |
| `title` | string | yes | 1–200 characters. |
| `category` | string | yes | `hike` \| `city` \| `food`. |
| `starts_at` | RFC 3339 | yes | |
| `ends_at` | RFC 3339 | yes | |
| `max_participants` | integer | yes | Includes the organizer. |
| `status` | string | yes | Always `open` for this event. |

The route geometry is deliberately **not** on the bus. It is large, only the
trip service needs it, and it is available over REST.

---

### `trip.status_changed`

The trip moved between lifecycle states — for example `draft` → `recruiting`,
or `recruiting` → `in_progress`. Emitted for **every** edge of the state
machine, terminal ones included, because it is written by the single function
that performs transitions and a rule with an exception in it is a rule somebody
gets wrong.

The terminal transitions also have events of their own — `trip.completed` and
`trip.cancelled` — because their consumers and payloads differ: this event says
only that the status moved, and `trip.completed` carries the roster the rating
window is opened from. A consumer that acts on the terminal states reads those;
one that only cares about the others filters on `new_status`.

- **Routing key:** `trip.status_changed`
- **Publisher:** trip
- **Consumers:** chat — posts a system message in the room; notification — notifies participants
- **Aggregate:** the trip
- **Outbox:** yes

```json
{
  "event_id": "4d5e6f70-8192-43a4-b5c6-d7e8f9012345",
  "event_type": "trip.status_changed",
  "event_version": 1,
  "occurred_at": "2026-09-12T06:00:00Z",
  "aggregate_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
  "payload": {
    "trip_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
    "organizer_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "old_status": "full",
    "new_status": "in_progress",
    "changed_at": "2026-09-12T06:00:00Z"
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `trip_id` | uuid | yes | Same as `aggregate_id`. |
| `organizer_id` | uuid | yes | |
| `old_status` | string | yes | `draft` \| `recruiting` \| `in_progress`. |
| `new_status` | string | yes | `recruiting` \| `in_progress` \| `completed` \| `cancelled`. |
| `changed_at` | RFC 3339 | yes | The moment the transition committed. |

The scheduled transitions carry the same payload as the organizer-driven ones
and are indistinguishable from them, on purpose: `recruiting → in_progress`
means the same thing to chat and to notification however it was triggered.

---

### `trip.completed`

The trip has finished. This is what opens the rating window, so the payload
carries the **full participant roster** — identity must be able to decide who
may rate whom without calling back into trip.

- **Routing key:** `trip.completed`
- **Publisher:** trip
- **Consumers:** identity — opens mutual rating between roster members; notification — sends the "rate your co-travellers" mail
- **Aggregate:** the trip
- **Outbox:** yes

```json
{
  "event_id": "5e6f7081-92a3-44b5-c6d7-e8f901234567",
  "event_type": "trip.completed",
  "event_version": 1,
  "occurred_at": "2026-09-13T18:00:00Z",
  "aggregate_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
  "payload": {
    "trip_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
    "title": "Carpathians ridge, two days",
    "organizer_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "started_at": "2026-09-12T06:00:00Z",
    "completed_at": "2026-09-13T18:00:00Z",
    "rating_window_closes_at": "2026-09-27T18:00:00Z",
    "participants": [
      {
        "user_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
        "role": "organizer",
        "joined_at": "2026-08-27T13:00:00Z"
      },
      {
        "user_id": "7d3f2e10-4c5b-4a69-8072-3e4f5a6b7c8d",
        "role": "participant",
        "joined_at": "2026-08-28T09:30:00Z"
      }
    ]
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `trip_id` | uuid | yes | Same as `aggregate_id`. |
| `title` | string | yes | Included so notification can write the email without a lookup. |
| `organizer_id` | uuid | yes | Also present in `participants` with role `organizer`. |
| `started_at` | RFC 3339 | yes | |
| `completed_at` | RFC 3339 | yes | |
| `rating_window_closes_at` | RFC 3339 | yes | After this, identity rejects new ratings for the trip. |
| `participants` | array of object | yes | Everyone who actually travelled. Removed participants are excluded. |
| `participants[].user_id` | uuid | yes | |
| `participants[].role` | string | yes | `organizer` \| `participant`. |
| `participants[].joined_at` | RFC 3339 | yes | |

This is the one deliberately fat payload in the catalogue. The alternative —
identity calling `GET /internal/trips/{id}/participants` — would make the
rating window depend on trip being up at that moment.

---

### `trip.cancelled`

The organizer called the trip off, or it expired without enough participants.

- **Routing key:** `trip.cancelled`
- **Publisher:** trip
- **Consumers:** chat — posts a system message and closes the room to new messages; notification — notifies everyone involved
- **Aggregate:** the trip
- **Outbox:** yes

```json
{
  "event_id": "6f708192-a3b4-45c6-d7e8-f90123456789",
  "event_type": "trip.cancelled",
  "event_version": 1,
  "occurred_at": "2026-09-01T10:00:00Z",
  "aggregate_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
  "payload": {
    "trip_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
    "title": "Carpathians ridge, two days",
    "organizer_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "reason": "organizer_cancelled",
    "comment": "Weather forecast is bad, moving to October.",
    "cancelled_at": "2026-09-01T10:00:00Z",
    "affected_user_ids": [
      "7d3f2e10-4c5b-4a69-8072-3e4f5a6b7c8d",
      "1e2d3c4b-5a69-4708-9182-a3b4c5d6e7f8"
    ]
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `trip_id` | uuid | yes | Same as `aggregate_id`. |
| `title` | string | yes | |
| `organizer_id` | uuid | yes | |
| `reason` | string | yes | `organizer_cancelled` \| `insufficient_participants` \| `expired`. |
| `comment` | string \| null | yes | Free text from the organizer; null for automatic cancellations. |
| `cancelled_at` | RFC 3339 | yes | |
| `affected_user_ids` | array of uuid | yes | Approved participants and pending requesters, excluding the organizer. |

---

### `trip.invite_sent`

The organizer invited a specific user to a trip directly, rather than waiting
for a join request.

- **Routing key:** `trip.invite_sent`
- **Publisher:** trip
- **Consumers:** notification — emails the invitee
- **Aggregate:** the trip
- **Outbox:** yes

```json
{
  "event_id": "708192a3-b4c5-46d7-e8f9-012345678901",
  "event_type": "trip.invite_sent",
  "event_version": 1,
  "occurred_at": "2026-08-28T08:00:00Z",
  "aggregate_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
  "payload": {
    "trip_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
    "title": "Carpathians ridge, two days",
    "invite_id": "c2b3a4d5-6e7f-4809-9a1b-2c3d4e5f6071",
    "inviter_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "invitee_id": "7d3f2e10-4c5b-4a69-8072-3e4f5a6b7c8d",
    "expires_at": "2026-09-04T08:00:00Z",
    "sent_at": "2026-08-28T08:00:00Z"
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `trip_id` | uuid | yes | Same as `aggregate_id`. |
| `title` | string | yes | |
| `invite_id` | uuid | yes | Idempotency handle for the invite itself. |
| `inviter_id` | uuid | yes | The organizer. |
| `invitee_id` | uuid | yes | Notification resolves the email address from its own user projection. |
| `expires_at` | RFC 3339 | yes | |
| `sent_at` | RFC 3339 | yes | |

---

### `join_request.created`

Someone asked to join a trip. The organizer needs to know.

- **Routing key:** `join_request.created`
- **Publisher:** trip
- **Consumers:** notification — notifies the organizer
- **Aggregate:** the trip
- **Outbox:** yes

```json
{
  "event_id": "8192a3b4-c5d6-47e8-f901-234567890123",
  "event_type": "join_request.created",
  "event_version": 1,
  "occurred_at": "2026-08-28T09:00:00Z",
  "aggregate_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
  "payload": {
    "join_request_id": "a1b2c3d4-e5f6-4708-9192-a3b4c5d6e7f8",
    "trip_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
    "title": "Carpathians ridge, two days",
    "organizer_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "requester_id": "7d3f2e10-4c5b-4a69-8072-3e4f5a6b7c8d",
    "message": "I have done this ridge before, happy to navigate.",
    "requested_at": "2026-08-28T09:00:00Z"
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `join_request_id` | uuid | yes | |
| `trip_id` | uuid | yes | Same as `aggregate_id`. |
| `title` | string | yes | |
| `organizer_id` | uuid | yes | The notification recipient. |
| `requester_id` | uuid | yes | |
| `message` | string \| null | yes | Up to 500 characters. |
| `requested_at` | RFC 3339 | yes | |

---

### `join_request.approved`

The organizer accepted a request. This is the event that grants chat access —
chat adds the member to the room, so an unapproved user can never open the
websocket.

- **Routing key:** `join_request.approved`
- **Publisher:** trip
- **Consumers:** chat — adds the member to the trip room; notification — tells the requester they are in
- **Aggregate:** the trip
- **Outbox:** yes

```json
{
  "event_id": "92a3b4c5-d6e7-48f9-0123-456789012345",
  "event_type": "join_request.approved",
  "event_version": 1,
  "occurred_at": "2026-08-28T09:30:00Z",
  "aggregate_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
  "payload": {
    "join_request_id": "a1b2c3d4-e5f6-4708-9192-a3b4c5d6e7f8",
    "trip_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
    "title": "Carpathians ridge, two days",
    "organizer_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "participant_id": "7d3f2e10-4c5b-4a69-8072-3e4f5a6b7c8d",
    "approved_at": "2026-08-28T09:30:00Z",
    "participant_count": 3,
    "max_participants": 8
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `join_request_id` | uuid | yes | |
| `trip_id` | uuid | yes | Same as `aggregate_id`. |
| `title` | string | yes | |
| `organizer_id` | uuid | yes | |
| `participant_id` | uuid | yes | The newly approved user. Gains chat access. |
| `approved_at` | RFC 3339 | yes | |
| `participant_count` | integer | yes | Count after approval, organizer included. |
| `max_participants` | integer | yes | |

Chat's handler is idempotent in two layers: `processed_events` for the
redelivery case, and an upsert on room membership, so a replayed approval
cannot duplicate a member.

---

### `join_request.rejected`

The organizer declined a request. No chat access is granted, so chat does not
consume this.

- **Routing key:** `join_request.rejected`
- **Publisher:** trip
- **Consumers:** notification — tells the requester
- **Aggregate:** the trip
- **Outbox:** yes

```json
{
  "event_id": "a3b4c5d6-e7f8-4901-2345-678901234567",
  "event_type": "join_request.rejected",
  "event_version": 1,
  "occurred_at": "2026-08-28T09:35:00Z",
  "aggregate_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
  "payload": {
    "join_request_id": "a1b2c3d4-e5f6-4708-9192-a3b4c5d6e7f8",
    "trip_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
    "title": "Carpathians ridge, two days",
    "organizer_id": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "requester_id": "1e2d3c4b-5a69-4708-9182-a3b4c5d6e7f8",
    "reason": "trip_full",
    "rejected_at": "2026-08-28T09:35:00Z"
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `join_request_id` | uuid | yes | |
| `trip_id` | uuid | yes | Same as `aggregate_id`. |
| `title` | string | yes | |
| `organizer_id` | uuid | yes | |
| `requester_id` | uuid | yes | The notification recipient. |
| `reason` | string | yes | `declined_by_organizer` \| `trip_full` \| `expired`. |
| `rejected_at` | RFC 3339 | yes | |

The organizer's free-text reason, if any, is intentionally not published — it
is visible in the trip API to the requester only.

---

### `participant.removed`

An approved participant left, or was removed by the organizer. Chat must
revoke room access; the user should not keep reading the group conversation.

- **Routing key:** `participant.removed`
- **Publisher:** trip
- **Consumers:** chat — removes room membership and closes any open websocket for that user
- **Aggregate:** the trip
- **Outbox:** yes

```json
{
  "event_id": "b4c5d6e7-f890-4123-4567-890123456789",
  "event_type": "participant.removed",
  "event_version": 1,
  "occurred_at": "2026-09-02T14:00:00Z",
  "aggregate_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
  "payload": {
    "trip_id": "b4d2f6a1-0c3e-4a58-9f77-1d5e8c2b4a90",
    "participant_id": "7d3f2e10-4c5b-4a69-8072-3e4f5a6b7c8d",
    "removed_by": "9c1e5b7a-2f34-4d60-8e91-0a2b3c4d5e6f",
    "reason": "removed_by_organizer",
    "removed_at": "2026-09-02T14:00:00Z",
    "participant_count": 2
  }
}
```

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `trip_id` | uuid | yes | Same as `aggregate_id`. |
| `participant_id` | uuid | yes | The user losing access. |
| `removed_by` | uuid | yes | The organizer, or the participant themselves when they left. |
| `reason` | string | yes | `left_voluntarily` \| `removed_by_organizer`. |
| `removed_at` | RFC 3339 | yes | |
| `participant_count` | integer | yes | Count after removal, organizer included. |

Message history stays in chat. Removal revokes access going forward; it does
not delete what the participant already wrote.
