# trip

Trips: the aggregate that everything else in TogetherGo hangs off. This stage
covers the data model, the status state machine, trip CRUD, discovery, the
participation flow — asking to join, being approved, leaving, being invited —
the transactional outbox relay that puts the resulting events on the bus, and
the two background workers — the lifecycle scheduler and the user projection
consumer. Ratings arrive later.

## Endpoints

Every endpoint requires a valid access token.

| Method | Path | |
| --- | --- | --- |
| `GET` | `/api/trips` | **discovery**: filter, search, keyset-paginate |
| `POST` | `/api/trips` | create; the trip is always a `draft` → 201 + `Location` |
| `GET` | `/api/trips/{id}` | the trip, its ordered route, its roster, its capacity and a `viewer` block |
| `GET` | `/api/trips/{id}/similar` | up to 5 nearby recruiting trips in the same category |
| `PATCH` | `/api/trips/{id}` | organizer only, `draft` or `recruiting` only |
| `POST` | `/api/trips/{id}/publish` | `draft` → `recruiting` |
| `POST` | `/api/trips/{id}/cancel` | → `cancelled`, from any non-terminal state |
| `DELETE` | `/api/trips/{id}` | organizer only, `draft` only, hard delete → 204 |
| `POST` | `/api/trips/{id}/requests` | ask to join, `{message?}` → 201 |
| `GET` | `/api/trips/{id}/requests` | **organizer only**: the queue, pending first, enriched |
| `GET` | `/api/trips/{id}/requests/me` | your own latest request, including a rejection's reason |
| `DELETE` | `/api/trips/{id}/requests/me` | withdraw your open request → 204 |
| `POST` | `/api/trips/{id}/requests/{rid}/approve` | organizer only; **the capacity invariant** |
| `POST` | `/api/trips/{id}/requests/{rid}/reject` | organizer only, `{reason?}` |
| `POST` | `/api/trips/{id}/participants/me` | leave a trip you joined |
| `DELETE` | `/api/trips/{id}/participants/{uid}` | organizer removes a participant |
| `POST` | `/api/trips/{id}/invites` | organizer invites users by id, `{user_ids: [...]}` |
| `GET` | `/api/trips/{id}/invites` | organizer only: who has been invited |
| `GET` | `/api/my/trips` | **the dashboard**: `?role=organizer` or `?role=participant` |
| `POST` | `/internal/scheduler/tick` | **debug only**: one lifecycle pass, synchronously |
| `GET` | `/healthz` | liveness, checks nothing |
| `GET` | `/readyz` | readiness, checks Postgres, identity's JWKS and the broker |

Client traffic reaches the service through the gateway on
`http://localhost:8080`, which routes `/api/trips` and `/api/my` here. Port 8002
is exposed in Compose for debugging only.

## Discovery — `GET /api/trips`

The highest-traffic read path in the system. Every parameter is optional, every
one is parsed into a typed struct before anything reaches SQL, and every
rejection is reported at once as a `validation_error`.

| Parameter | |
| --- | --- |
| `date_from`, `date_to` | RFC 3339; matches trips **overlapping** the window |
| `near_lat`, `near_lng`, `radius_km` | departure within radius; **all three or none**, 1–500 km |
| `dest_lat`, `dest_lng`, `dest_radius_km` | destination within radius; all three or none |
| `min_days`, `max_days` | trip duration in days, rounded up, never below 1 |
| `min_free_slots` | `capacity - approved_count >= n` |
| `categories` | comma-separated, from the allowlist |
| `q` | full text over title and description |
| `limit` | 1–50, default 20 |
| `cursor` | opaque; base64 of `<start_at RFC3339Nano>\|<uuid>` |

Two rules are applied to every discovery query and are **not** parameters:
`status = 'recruiting'`, and `start_at` in the future. They live in one constant
in `internal/store/search.go`, so there is no combination of filters that
reveals a draft or a trip that has already left. The fixture deliberately seeds
a draft, a cancelled, a completed and a departed trip that all match the
narrowest filters the tests use, so a query that lost a rule fails loudly.

Each item carries the trip's scalars, `free_slots`, named `departure` and
`destination`, a route summary capped at six stop names, and an `organizer`
block. The ellipsis is a `truncated` boolean, not a `"…"` appended to the list:
a client must never have to decide whether the last element is a place or a
piece of punctuation.

### The query shape

Four decisions, all of them about the plan rather than the code:

**Radius filters are `ST_DWithin`, never `ST_Distance`.** `ST_DWithin` is an
indexable operator — the planner rewrites it into a bounding-box search that
`trips_departure_gist` answers, then rechecks the survivors exactly.
`ST_Distance(a, b) < r` is a function call evaluated on every row the scan
already produced. The two select the same rows; only one of them can be served
by an index, and the difference is visible only in the plan:

```
ST_DWithin   ->  Bitmap Index Scan on trips_departure_gist
                 Index Cond: (departure_location && _st_expand(..., 50000))

ST_Distance  ->  Filter: (st_distance(departure_location, ...) < 50000)
                 ...and no mention of trips_departure_gist at all
```

`TestRadiusPredicateUsesTheGistIndex` asserts exactly that, taking both plans
with `enable_seqscan = off` so the result is not the planner preferring a scan
over a few dozen seeded rows: given every chance to use the GIST index, the
`ST_Distance` form still cannot. To see it by hand:

```sh
make psql-trip
```
```sql
SET enable_seqscan = off;
EXPLAIN SELECT id FROM trips
WHERE status = 'recruiting' AND start_at > now()
  AND ST_DWithin(departure_location,
                 ST_SetSRID(ST_MakePoint(24.7111, 48.9226), 4326)::geography,
                 50000);
```

**Free text is a `tsvector`, never `ILIKE`.** `trips.search_vector` is a
`GENERATED ALWAYS AS ... STORED` column over title (weight A) and description
(weight B), with a GIN index — migration `00002`. A generated column rather than
a trigger, because a trigger is a second writer that has to be remembered on
every future write path, and a generated column cannot drift from what it is
derived from. Queries go through `websearch_to_tsquery`, which understands
quoted phrases, `or` and a leading `-`, and cannot be made to raise a syntax
error by a user typing an unbalanced parenthesis into a search box.
`ILIKE '%q%'` cannot use an index at all and matches the wrong things anyway:
no stemming, so "hike" never finds "hiking".

The configuration is `english` over a mixed Ukrainian/English corpus. English
words stem and English stop words drop, which is what a search box needs for the
half of the corpus Postgres understands; Cyrillic tokens pass through the
snowball stemmer essentially unchanged and are indexed as written — no worse
than `simple` would have managed, and better everywhere else.

**Pagination is keyset, never `OFFSET`.** The cursor is base64 of
`<start_at RFC3339Nano>|<uuid>` and the predicate is a row comparison,
`(start_at, id) > ($1, $2)` — the same pair, in the same order, as the trailing
columns of `trips_status_start_idx`, so page 40 costs what page 1 costs. The
`id` is what makes the ordering total: `start_at` alone is not unique, so a
cursor holding only the timestamp either skips rows (`>`) or repeats them
forever (`>=`). `TestSearchPaginationAcrossTiedStartTimes` seeds twenty trips
at three instants and walks them at `limit=7`, with the page boundaries falling
inside a group that shares a `start_at`.

`RFC3339Nano`, not `RFC3339`: `timestamptz` keeps microseconds, and a cursor
truncated to the second would re-read every row sharing that second with the
last row of the page. Decoding is defensive at every step — length, alphabet,
separator, timestamp, uuid — and a malformed cursor is a 400 naming the `cursor`
field, never a panic.

**The route summary is joined onto the page, not onto the candidate set.**
Selecting and ordering happen in a CTE with the `LIMIT` already applied; the
`LATERAL` that reads `trip_points` runs against its output. A page of twenty
trips reads twenty routes, whatever the filters matched.

### The organizer block

Users belong to identity, and there is no join across that boundary (CLAUDE.md
rule 1). Every page collects its distinct organizer ids and resolves them
**projection first**: one indexed read against the local `user_ref` table, then
**one** call to `GET /internal/users` for whatever the projection did not have —
identity offers no single-id variant precisely so that nobody writes the loop —
and a backfill so the next page is local too. See **The user projection** for
why both halves are kept.

Behind that call, resolved users are cached in Redis for 60 seconds, keyed per
user rather than per page, which is what makes a cold projection's second page
mostly cache hits. Sixty seconds matches the `Cache-Control: max-age=60`
identity puts on the same response, so a renamed user is stale for the same
bounded interval platform-wide.

**A search page never 500s because identity is down.** With the projection warm
it does not even notice; and if both the projection read and the call fail, the
page is served with organizer blocks carrying the id and `null` everywhere else,
and the outage is a warning in this service's log. The same shape is returned
for a deleted account, and the two are indistinguishable to a client on purpose:
in both cases the only thing it can do is render a placeholder. A cache that is
down is a slower page rather than a failed one — neither `Cache.Get` nor
`Cache.Put` returns an error, because "carry on anyway" is the only thing a
caller could do with one.

## The dashboard — `GET /api/my/trips`

Discovery answers "what is out there". This answers "where am I", and the
difference is not a filter. Discovery is scoped in SQL to `status = 'recruiting'
AND start_at > now()` and its parameter allowlist is closed, so there is no
query against `/api/trips` that returns your drafts, your finished trips or a
trip you have only applied to. Those are the three things `/my-trips` exists to
show.

| Parameter | |
| --- | --- |
| `role` | **required**, exactly `organizer` or `participant`; anything else is 400 |
| `status` | comma-separated trip statuses; every status is accepted here, `draft` included |
| `limit` | 1–50, default 20 |
| `cursor` | opaque, from the previous page's `next_cursor` |

Who the caller is comes from the token's `sub`. There is no "whose dashboard" to
get wrong, and no parameter that could be made to name somebody else.

**`role=organizer`** is every trip whose `organizer_id` is the caller, in every
status. It is the only listing in the service that returns a draft or a
cancelled trip, and it is allowed to be because there is exactly one person it
can return them to. Each row additionally carries `pending_requests_count`, from
a `LATERAL` over `join_requests` — the dashboard renders the inline
approve/reject list from it without a second round trip.

**`role=participant`** is the union of two sets, because the "I participate" tab
shows trips the caller has only applied to:

| | `membership_status` |
| --- | --- |
| a `participants` row with `role = 'participant'` | `approved` |
| a `join_requests` row with `status = 'pending'` | `requested` |

Trips the caller organizes are excluded — their organizer `participants` row
would otherwise put every trip they run on both tabs — and so is any draft,
which on this tab could only ever be somebody else's.

The union is collapsed to one row per trip before anything else happens.
Somebody who was approved, left and applied again matches both branches, and a
duplicate would be wrong on its face *and* fatal to the cursor: the two copies
share a `(start_at, id)` that no comparison can separate. `min(rank)` makes the
set one row per trip by construction and picks the stronger standing.

### Ordering and the cursor

`ORDER BY start_at DESC, id DESC` — the reverse of discovery's, because this is
a history and not a feed of things you can still join. The cursor is the same
opaque base64 `(start_at, id)` pair discovery uses; only the comparison operator
flips, and that belongs to the query rather than to the token.

Both the keyset predicate and the `ORDER BY` are applied to the *outer* query,
over the union, never per branch. Applied per branch, each would resume from its
own position and the merged page would skip whichever branch the cursor did not
come from — a gap that appears only once a page happens to end on a row from the
other set, which is to say in production and not in a two-row test.
`TestMyTripsPaginatesTheUnionWithoutGapsOrDuplicates` walks a seven-trip mixed
union at `limit=2` and asserts the walk equals the unpaginated answer.

### The card

The same `tripListItemResponse` discovery serves, plus two keys. Both are
**optional and omitted** from a search result rather than null in it: `GET
/api/trips` is a shipped contract and a client that has never heard of them must
keep working. `pending_requests_count` uses `omitempty` on a pointer, so an
organizer with an empty queue still gets a `0` — a number to render, not a
missing key to infer from.

## `GET /api/trips/{id}` — the viewer block

The trip detail page has four mutually exclusive primary actions — "Request to
join", "Requested", "Chat", "Leave" — and without this it would have to guess at
three of them or spend two more round trips finding out:

```json
"viewer": {"is_organizer": false, "is_participant": true, "join_request_status": null}
```

`is_participant` is true for the organizer too: they hold a seat and have a
`participants` row, which is the same roster the response carries. A client
reads `is_organizer` first.

`join_request_status` is only ever `pending`, `rejected` or `null`. An approved
request has become the `participants` row and `is_participant` is the answer; a
withdrawn one puts the caller back where they started, which is the same
position as never having asked. It is present-and-null rather than absent, so no
client has to tell a missing key from a null one.

The query runs *after* the visibility check. A caller who may not know a trip
exists learns nothing about it, including what their own application to it did.
This is the only trip response that carries the block — the mutations that
return a trip are all acts whose outcome the caller already knows.

## `GET /api/trips/{id}/similar`

Up to five recruiting trips in the same category whose departure is within
100 km, excluding the trip itself. Ordered by departure time, not by distance:
every candidate is already inside the radius, so "which of these can I still
join, soonest" is the question the panel exists to answer — and ordering by
distance would mean an `ST_Distance` per candidate, which is the cost the
`WHERE` clause is careful to avoid.

Visibility is applied to the *source* trip exactly as `GET /api/trips/{id}`
applies it: asking what is similar to somebody else's draft answers 404.

## The participation flow

```
        POST /requests                 approve                    leave / remove
user  ───────────────►  pending  ─────────────────►  approved  ───────────────►  (off the trip,
                           │                          + a seat                    seat freed)
                           ├──── reject ────►  rejected  ──── may apply again ───►
                           └──── DELETE /requests/me ────►  cancelled
```

A user may re-apply after a rejection but may never have two open requests. That
is one partial unique index and not a line of Go:

```sql
CREATE UNIQUE INDEX join_requests_one_pending_idx
    ON join_requests (trip_id, user_id)
    WHERE status = 'pending';
```

A plain `UNIQUE (trip_id, user_id)` would make the second application impossible
rather than the second *open* one, and would throw away the history of the first.

Four ways of being refused, four codes, because each has a different remedy and
a client switches on the code, never on the message:

| Situation | Status | `code` |
| --- | --- | --- |
| it is your own trip | 409 | `cannot_join_own_trip` |
| the trip is a draft / in progress / cancelled / completed | 409 | `trip_not_recruiting` |
| you are already on the trip | 409 | `already_participant` |
| you already have an open request | 409 | `request_already_pending` |
| the last seat went to somebody else | 409 | `trip_full` |
| the request has already been decided | 409 | `request_not_pending` |
| the organizer cannot leave or be removed | 409 | `cannot_remove_organizer` |
| the trip is over; its roster is history | 409 | `trip_ended` |

A **draft** answers 404 to a stranger before any of these, exactly as `GET`
does: a 403 would confirm that the id names a real trip.

Cancelling and rejecting never touch `approved_count` — a pending request never
held a seat, which is the whole reason the counter moves at approval time.
Removing a participant decrements it and emits `participant.removed`; leaving
emits the same event with `reason: left_voluntarily` and `removed_by` set to the
person who left, because chat has to revoke access either way and those two
fields are what tell the cases apart.

### The capacity invariant

Everything in `Store.ApproveJoinRequest` happens in **one transaction**, in this
order:

1. `SELECT the trip ... FOR UPDATE`
2. the trip is `recruiting` and `approved_count < capacity`
3. the request is still `pending`
4. the request becomes `approved`
5. a `participants` row appears
6. `approved_count` goes up by one
7. `join_request.approved` goes into the `outbox`

Step 1 is what makes steps 2 to 7 mean anything. Twelve concurrent approvals all
reach it, Postgres lets one past at a time, and each one that gets the lock
re-reads an `approved_count` that already includes its predecessors' commits.
There is no window between "there is room" and "the seat is taken" for a second
transaction to look into.

**There is no application-level mutex, here or anywhere in the package.** A
mutex would pass the test below and fail the moment a second replica started,
because it coordinates goroutines and the thing that needs coordinating is
transactions. Underneath it all, `trips_approved_fits` — the CHECK constraint
from migration `00001` — is the backstop: if this method were ever wrong, the
database would refuse the write rather than record an overbooked trip.

`TestConcurrentApprovalsRespectCapacity` fires N+5 goroutines at a trip with N
free seats, released together from a barrier, and asserts that exactly N
succeed, that the other five get `trip_full`, that the roster and the counter
agree, and that exactly N `join_request.approved` rows reached the outbox — a
rolled-back approval must not leave an event behind claiming somebody joined.

```sh
make test-trip-race     # go test ./internal/store/ -run TestConcurrent -race -count=10
```

Ten runs, because a lock bug that shows up one time in three would pass a single
run often enough to be believed.

### The organizer's queue

`GET /api/trips/{id}/requests` is pending first, newest first within each group:
a decided request is history and must not push a live one below the fold, and a
queue read on a phone is read from the top. Keyset-paginated like everything
else, with a cursor that carries the pending/decided rank as well as
`(created_at, id)` — without the rank, the first row of the second page would be
compared against a timestamp from the wrong group.

Each row carries the requester's name, photo, rating and rating count. Those
belong to identity and this service holds no copy of them (CLAUDE.md rule 1), so
the page collects its distinct user ids and makes **one** call to identity's
batch resolver — the same arrangement discovery uses for organizers, including
the degraded path: if identity is down the queue still renders, with ids and
nulls, and the organizer approves an id. `rating_count` is on this block and not
on the organizer block because this is the screen where it changes a decision:
4.9 from one rating and 4.9 from forty are not the same recommendation.

### Invitations

`POST /api/trips/{id}/invites` is all or nothing. Twenty ids where one is
already invited writes no rows and answers 409 naming that one in
`details.user_ids`, rather than nineteen invitations and a silence about the
twentieth. A repeated id inside one request is deduplicated rather than
rejected — the same person named twice has an obvious intent — and the batch
limit is counted after that.

Invitations are only possible on a `recruiting` trip: an invitation to a draft
is a link to a 404. `expires_at` is derived from `created_at` plus a TTL held in
code, not stored, so changing the policy is a deploy rather than a data
migration.

## Events and the outbox

Nine event types leave this service, every one of them through the
transactional outbox (CLAUDE.md rule 4):

| Event | Goes to |
| --- | --- |
| `trip.created` | chat |
| `trip.cancelled` | chat, notification |
| `join_request.created` | notification |
| `join_request.approved` | chat, notification |
| `join_request.rejected` | notification |
| `participant.removed` | chat |
| `trip.invite_sent` | notification |
| `trip.status_changed` | chat, notification |
| `trip.completed` | identity, notification |

One arrives: `trip.user-events` carries `user.profile_updated` and
`user.rating_updated` from identity into the local user projection. See
**The user projection** below.

The domain write and the outbox insert share a transaction, so "the participant
was approved" and "the `join_request.approved` event exists" are one atomic fact
rather than two. **Nothing publishes inline from a handler.** A broker ack
followed by a failed commit would invent an event for a thing that did not
happen; a commit followed by a failed publish would lose one for a thing that
did.

`join_request.approved` is the fattest payload here on purpose. Chat creates the
room membership and notification writes the mail, and neither may call back into
this service to do it — a callback would make the side effect depend on trip
being up at that moment, which is exactly what putting the fact on a durable bus
was meant to avoid. So it carries the trip id, the trip title, the organizer,
the newly approved participant and the counts.

The organizer's free-text rejection reason is deliberately **not** on the bus.
`join_request.rejected` carries a reason *code* (`declined_by_organizer`), which
is what a notification template can act on; the sentence lives in
`join_requests.decision_reason` and is readable only through
`GET /api/trips/{id}/requests/me`, by the person it was written to.

### The relay

`internal/events/relay.go`, a goroutine started from `main` with a context of
its own. Every tick, in one transaction:

```sql
SELECT id, aggregate_id, event_type, payload, created_at
FROM outbox
WHERE published_at IS NULL
ORDER BY created_at
LIMIT 100
FOR UPDATE SKIP LOCKED;
```

then publish each row to `togethergo.events` with routing key `event_type`,
`delivery_mode: 2` and **publisher confirms on**, wait for the confirms, `UPDATE
published_at`, commit. Dying anywhere before the commit rolls the batch back
with the rows still `NULL`, and the next tick republishes them — which is
precisely where at-least-once delivery comes from, and why `event_id` is the
stable `outbox.id` rather than something minted at publish time.

`FOR UPDATE SKIP LOCKED` is what makes replicas safe: a second relay running the
same query at the same instant skips these rows and takes the next hundred.
Scaling this service to three instances needs no leader election and no
coordination code.

Four more properties, each of them a line in the acceptance criteria:

- **Nothing is dropped.** A publish failure rolls the batch back and the next
  tick retries it, with the delay doubling from 500 ms up to a 30-second
  ceiling and resetting on the first success. There is no per-row attempt
  counter, because the rows are still in the table: "retry" and "the next tick"
  are the same thing.
- **A broker restart is not an outage.** The connection is re-established
  lazily from inside the publish loop. `TestEventsSurviveABrokerRestart` stops
  the RabbitMQ container, approves three requests against a service that keeps
  working, asserts the three events are still sitting unpublished in the outbox,
  starts the container again and waits for all three to arrive. No restart of
  this service is involved, which is the point.
- **Shutdown finishes the batch in flight.** SIGTERM stops the loop from
  *starting* another batch; the one under way runs to completion on a detached
  context, because abandoning it between "the broker confirmed these hundred"
  and "the rows are marked published" would republish the lot on the next boot.
  Then the AMQP channel closes. `main` orders it deliberately: stop accepting
  requests, drain them, and only then stop the thing that publishes what they
  wrote.
- **Published rows are pruned.** A daily sweep deletes rows published more than
  seven days ago, in bounded chunks so the job never takes a long lock.
  Unpublished rows are never deleted however old they are: one still `NULL`
  after a week is a stuck event and wants a human, not a `DELETE`.

The relay declares no topology — no exchange, no queue, no binding. The broker
imports all of it at boot from `deploy/rabbitmq/definitions.json`, and a service
that declared its own would sooner or later declare it with different arguments
and earn a `PRECONDITION_FAILED`.

A trip service that starts before RabbitMQ starts anyway: the broker is
contacted lazily, requests are served, and events wait in the outbox until it
appears. `/readyz` reports `broker: unavailable` while that is true.

## The lifecycle scheduler

`internal/scheduler`, a goroutine started from `main`, ticking every
`TRIP_SCHEDULER_INTERVAL` seconds (60 by default). Two passes per tick:

```
recruiting  ──► in_progress    where start_at <= now
in_progress ──► completed      where end_at   <= now
```

In that order, so a trip left behind by an outage — both its start and its end
in the past — is caught up completely by a single pass rather than over two
ticks.

**Every transition goes through the same code the organizer's own publish and
cancel go through.** `Store.ChangeStatus` is the authorising wrapper the HTTP
handlers call; `store.changeStatus` is the transactional core underneath it, and
it is the single writer of `trips.status`. The scheduler cannot use the wrapper
— it is nobody's organizer, and it holds its batch's row locks in a transaction
the wrapper's own would deadlock against — so it calls the core from inside that
transaction. There is one `UPDATE trips SET status` in the service and one
consultation of `domain.Can`, and both workers reach them.

Every transition also writes a `trip.status_changed` outbox row, in that same
transaction. That includes the terminal edges: `contracts/events.md` used to say
they never appeared here, and it now says they do, because "one row per
transition, from the one function that performs them" is a rule with no
exception to get wrong. Consumers that only care about the non-terminal edges
filter on `new_status`.

Three edges write a second event beside it, because their consumers need
something the status change does not carry:

| Edge | Also writes | Why |
| --- | --- | --- |
| `draft → recruiting` | `trip.created` | chat provisions the room from the title, up front, so the organizer has somewhere to post before anyone joins |
| `* → cancelled` | `trip.cancelled` | chat closes the room; notification has to reach everybody affected, and the payload carries who that is |
| `in_progress → completed` | `trip.completed` | identity opens the rating window from the roster it carries |

`trip.created` is emitted on **publication**, not on the INSERT. A draft is
visible to nobody but its organizer, and provisioning a chat room for one would
create rooms for trips that are never announced. The edge is only reachable from
`draft`, so it cannot fire twice for one trip.

`trip.cancelled`'s `affected_user_ids` is the approved participants *and* the
people whose join request was still open — the second group is easy to forget
and they are waiting for an answer that is now never coming. The organizer is
excluded: they are the one who caused it.

**Two replicas are safe with no coordination.** The claim query is:

```sql
SELECT ... FROM trips
WHERE status = 'in_progress' AND end_at <= $1
ORDER BY end_at
LIMIT $2
FOR UPDATE SKIP LOCKED;
```

A second scheduler running this at the same instant does not block on these rows
and does not fail on them — it skips them and takes the next batch. No leader
election, no advisory lock. The lock is held for the whole transaction, which is
why the transition runs inside it: claiming ids, committing, and *then*
transitioning would open exactly the window `SKIP LOCKED` was chosen to close.

### `trip.completed`, exactly once

The completion edge additionally writes `trip.completed`, carrying the **full
roster**. Identity opens the rating window from it and notification writes the
"rate your co-travellers" mail, and neither may call back into this service for
the participants — a callback would make the rating window depend on trip being
up at that moment, which is what putting the fact on a durable bus was meant to
avoid.

Two guards, and they are not the same guard twice:

- **`trips.completed_event_emitted`**, flipped in the same statement that tests
  it, inside the transition's transaction. The status check very nearly
  suffices — `completed` is terminal, so the edge cannot be walked twice — but
  "very nearly" is not the right strength of claim about the one event that
  opens a rating window, and the column is what makes a future replay, backfill
  or hand-run of the scheduler safe by construction.
- **A roster of one.** A trip that reaches `end_at` with only its organizer
  aboard still completes, and emits no `trip.completed` at all. There is nobody
  to rate, and a window in which one person may rate themselves is a mail that
  wastes an afternoon. The column stays `false` for such a trip: it records what
  happened, not what was considered.

### `POST /internal/scheduler/tick`

One pass, run synchronously, answering `{"started":n,"completed":n,
"completed_events":n}`. It exists so tests do not have to sleep: asserting that a
trip whose `end_at` has passed becomes `completed` otherwise means waiting out a
tick interval, and a suite that sleeps is either slow or flaky. It is also the
thing to reach for in an incident, when the question is "did the scheduler stop,
or is there genuinely nothing due?".

It is not a client endpoint. `deploy/traefik/dynamic.yml` routes no `/internal`
prefix, which is the first lock; `X-Internal-Token` is the second, because in
Compose every container shares one network and "not routed" is a statement about
Traefik rather than about who can open a socket to 8002. A caller without the
secret gets **404**, not 401 — it should not learn the endpoint is there. And
the route is not registered at all unless `TRIP_SCHEDULER_DEBUG_TICK` is on,
which it is not in production.

## The user projection

`internal/events/consumer.go` consumes `trip.user-events`
(`user.profile_updated`, `user.rating_updated`) and projects it into `user_ref`,
a local table holding each user's display name, photo and aggregate rating.

Reads go **projection first, identity second**:

```
user_ref  ──miss──►  GET /internal/users  ──►  backfill user_ref
   │                  (behind the 60 s Redis cache)
   └──hit──► render
```

This is a deliberate hybrid and both halves earn their place.

**The projection makes the hot read path local.** A discovery page used to cost
an HTTP call to another service; now it costs one indexed `SELECT` against a
table in this service's own database. It also keeps working while identity is
down or restarting, which the sixty-second cache in front of the HTTP client only
managed for users somebody happened to have looked at recently.

**The fallback means a gap degrades rather than breaks.** A projection is only
as complete as the events it has consumed. A cold start, a queue purged during
an incident, a user created while this service was down — each leaves ids the
projection has never heard of, and without the fallback the page would render a
blank name for them *permanently*, until some unrelated profile edit happened to
fill it in. With it, the first page that needs such a user pays one HTTP call,
backfills the row, and every page after that is local again. **A missed event
costs a slower correct answer instead of a wrong one.**

Failure of both is still not a failure of the page: `Resolve` returns what it
has together with the error, and the handlers log it and render the rest. That
rule is older than the projection and is not weakened by it.

### The table

```sql
CREATE TABLE user_ref (
    user_id      uuid PRIMARY KEY,
    full_name    text,
    photo_url    text,
    rating_avg   numeric(3,2),
    rating_count integer NOT NULL DEFAULT 0,
    profile_updated_at timestamptz,
    rating_updated_at  timestamptz,
    updated_at   timestamptz NOT NULL DEFAULT now()
);
```

Three things about it are not obvious:

- **`full_name` is nullable, and a `NULL` is not a projection hit.** A rating
  event for a user no profile event has covered creates a row with no name.
  `Store.UserRefs` filters those out, so the id falls through to identity and
  comes back filled in. Storing the rating anyway is better than dropping it —
  the name is one HTTP call away, and the rating would otherwise be lost until
  the next rating event.
- **Two watermarks, not one.** `contracts/events.md` requires a consumer to
  discard an update older than the one it has stored. Profile updates and rating
  updates carry independent timestamps from independent causes, so a single
  watermark would let a rating recorded at 12:05 silently reject a profile edit
  made at 12:03 and delivered afterwards — a name that stays wrong until the
  user edits it again. Each kind is compared only against its own.
- **The backfill fills gaps and never overwrites.** A field group whose
  watermark is `NULL` has never been told anything by an event and is filled
  from identity's snapshot; one that has is left exactly as it is. Both sources
  are the same data from the same owner, but only the event stream carries a
  timestamp this service can order against, so the stream wins by default.

### Consuming

Idempotency is `processed_events (event_id uuid primary key, processed_at
timestamptz)` in this service's own database (CLAUDE.md rule 5). The
`INSERT ... ON CONFLICT DO NOTHING` and the projection write are one
transaction, and the insert comes first so it takes the row lock before any work
happens. A redelivery — and there will be redeliveries, because delivery is
at-least-once by contract — inserts nothing, applies nothing and is acked.

The consumer declares no topology, for the same reason the relay does not: the
broker imports the queue and its bindings at boot from
`deploy/rabbitmq/definitions.json`.

Failure handling follows `contracts/events.md` exactly:

| Situation | What happens |
| --- | --- |
| Applied, or a duplicate | ack |
| An `event_type` this build does not know | log at `warn`, mark processed, ack |
| Body is not an envelope, no `event_id`, payload malformed | nack, no requeue → `trip.user-events.dlq` |
| Transient failure (database down, deadlock) | 3 retries in-handler at 200/400/800 ms ±20 % jitter, then the DLQ |

**Never `requeue=true`.** A requeue puts the message back at the head of the same
queue, it is redelivered at once, it fails again, and the consumer spins at full
speed on a poison message. Retrying happens in-process where the backoff is under
our control. The DLQ is a human queue — nothing drains it on a timer, and
replaying from it is safe because `processed_events` makes a replay a no-op for
anything already handled.

An unknown event type is deliberately *not* poison. It is a message meant for a
newer version of this service, and dead-lettering it would fill a DLQ with
messages nobody will ever act on.

## The status state machine

```
draft ──────► recruiting ──────► in_progress ──────► completed
  │                │                   │
  └────────────────┴───────────────────┴──────────► cancelled
```

`completed` and `cancelled` are terminal. A status is never allowed to
transition to itself: publishing an already-recruiting trip is a stale client,
not a no-op, and answering 409 is what tells it to refetch.

Who takes which edge:

| Edge | Driven by |
| --- | --- |
| `draft → recruiting` | the organizer, `POST /publish` |
| `recruiting → in_progress` | the scheduler, once `start_at` has passed |
| `in_progress → completed` | the scheduler, once `end_at` has passed |
| `* → cancelled` | the organizer, `POST /cancel` |

The table lives in `internal/domain/status.go` as an explicit
`map[Status][]Status`, and `domain.Can(from, to)` is the only thing that reads
it. **Every status write goes through `store.changeStatus`** — the core that
`Store.ChangeStatus` authorises for the HTTP handlers and that the scheduler
calls from inside its own transaction. It consults `Can` while the trip is row
locked, writes the status, and writes the `trip.status_changed` outbox row; no
handler and no worker builds its own `UPDATE`. Creation is not a transition — it
inserts `domain.StatusInitial` and has no "from" to check.

`internal/domain/status_test.go` walks all 25 ordered pairs, so both halves of
the contract are covered: the six legal edges and the nineteen refusals.

## Validation

| Field | Rule |
| --- | --- |
| `title` | 3–120 characters, counted in runes |
| `description` | optional, ≤ 4000 characters |
| `category` | one of `nature`, `city`, `abroad`, `hiking`, `food`, `other` |
| `capacity` | 2–50, and never below `approved_count` |
| `start_at` | in the future (on create, and on any edit that moves it) |
| `end_at` | not before `start_at`, and at most 60 days after it |
| `points` | 2–20, ordered; `lat` ∈ [-90, 90], `lng` ∈ [-180, 180] |
| | no two *consecutive* points share coordinates — a route that returns to its start is fine |

Every failure is reported at once, as
`{"error": {"code": "validation_error", "details": {"fields": [{"field": "points[2].lat", ...}]}}}`.
A form that gets one error per round trip takes as many round trips as it has
mistakes.

`PATCH` is a genuine partial update: an omitted key changes nothing,
`"description": null` clears the description, and `points`, if present, replaces
the whole route. The patch is merged onto the stored trip and the *result* is
validated — moving only `end_at` can still break "end_at must not be before
start_at", which is a statement about the trip and not about the request.

## Visibility

A draft is the organizer's private workspace. To everyone else it does not
exist, so an unauthorised read answers **404, not 403** — a 403 would confirm
that the id names a real trip. Once published, the trip is public and a
non-organizer trying to change it gets the honest 403.

`GET /api/my/trips?role=organizer` is the one listing that returns drafts, and
it does not weaken this: the rule it applies in place of discovery's scope is
`organizer_id = you`, which is stricter, not looser.

## The denormalised endpoints

`trips.departure_location` and `trips.destination_location` are copies of the
first and last `trip_points` row. They exist so the search radius filter is
`ST_DWithin(departure_location, $1, $2)` — a plain indexed predicate on the table
being scanned, answered by `trips_departure_gist` — rather than a subquery
against `trip_points`.

The duplication is safe because it is never written on its own: `insertPoints`
is the only writer of `trip_points`, and it rewrites both columns in the same
transaction. `TestCreateTripDenormalisesTheEndpoints` and its update counterpart
assert the two agree by comparing them in SQL, not in Go.

## Authentication

Tokens are verified here, against identity's JWKS at `JWKS_URL` — cached for ten
minutes, refetched when a token carries an unknown `kid` (the key-rotation path),
and rate-limited to one refetch per ten seconds so that forged `kid` values
cannot turn this service into a load generator pointed at identity. RS256 is
pinned in the parser and never read from the token's own header.

A gateway-injected identity header is not trusted and not read: in Compose every
container shares a network with every other, so such a header proves nothing
(CLAUDE.md rule 6).

## Running it

From the repository root:

```sh
make up            # brings the stack up, trip included
make migrate-trip  # migrations never run on container startup
```

Locally, without a container:

```sh
cd services/trip
export TRIP_DATABASE_URL='postgres://trip_user:trip_pass@localhost:5432/trip_db?sslmode=disable'
export JWKS_URL=http://localhost:8001/.well-known/jwks.json

go run ./cmd/migrate up
go run ./cmd/server
```

`cmd/migrate` carries the migrations embedded, so it is a single static binary
and needs no copy of `migrations/` beside it. It supports `up`, `up-by-one`,
`down`, `reset`, `status` and `version`.

## Configuration

| Variable | Default | |
| --- | --- | --- |
| `TRIP_DATABASE_URL` | — | required; pgx DSN for `trip_db` as `trip_user` |
| `JWKS_URL` | — | required; identity's key document |
| `JWKS_CACHE_TTL` | `600` | seconds |
| `IDENTITY_INTERNAL_URL` | `http://identity:8001` | origin of `/internal/users`; never the gateway |
| `INTERNAL_API_TOKEN` | — | required; sent as `X-Internal-Token` |
| `IDENTITY_TIMEOUT_MS` | `2000` | past this, a page renders with null organizer names |
| `REDIS_URL` | — | optional; unset means no user cache |
| `USER_CACHE_TTL` | `60` | seconds; matches identity's own `max-age` |
| `RABBITMQ_URL` | — | required; the broker the outbox relay publishes through |
| `RABBITMQ_EXCHANGE` | `togethergo.events` | named, never declared |
| `TRIP_OUTBOX_POLL_MS` | `500` | how often the relay looks for unpublished rows |
| `OUTBOX_RETENTION_DAYS` | `7` | how long published rows are kept |
| `TRIP_SCHEDULER_INTERVAL` | `60` | seconds between lifecycle passes |
| `TRIP_SCHEDULER_DEBUG_TICK` | on unless `ENVIRONMENT=production` | registers `POST /internal/scheduler/tick` |
| `TRIP_PORT` | `8002` | |
| `TRIP_DB_MAX_CONNS` | `10` | pgx pool size |
| `SHUTDOWN_TIMEOUT` | `15` | seconds to drain on SIGTERM |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `ENVIRONMENT` | `local` | |

`config.Load()` is the only place the environment is read, and it reports every
problem at once rather than one restart per variable.

## Tests

```sh
make test-trip     # or: go test ./...
```

`internal/domain`, `internal/auth` and `internal/config` need nothing but Go.
`internal/store` and `internal/http` start a `postgis/postgis:16-3.4` container
per package with testcontainers-go and run the service's own goose migrations
against it — so a migration that does not apply cleanly fails the suite rather
than being discovered in Compose. `TestMigrationsAreReversible` rolls everything
back and reapplies it, which is the "`goose up`/`down` are both clean" check.
`internal/users` starts a `redis:7-alpine` for the cache tests, because what is
being asserted is that `MGET` reports misses positionally and that `SET` carries
a TTL — a fake that agreed with the code would prove neither.

`internal/events` starts a `rabbitmq:3.13-management` **with the repository's own
`deploy/rabbitmq/definitions.json` and `rabbitmq.conf` mounted**, so the
exchange, queues and bindings under test are the ones Compose runs rather than a
topology the test invented to agree with itself. That is what lets
`TestEventsReachTheQueuesTheirBindingsName` assert that `participant.removed`
reaches chat and not notification — a fact that lives in the bindings file, not
in this service's code. The container's host port is pinned rather than mapped
dynamically, because the restart test stops and starts it and the relay under
test holds one URL for its whole life.

The events tests are an external test package (`package events_test`): they
write the outbox through `internal/store`, which imports `internal/events`, so
an internal test file would be an import cycle. The consumer tests publish into
that same real broker and assert on the real `user_ref` table, so what they
cover includes the topology — that `user.profile_updated` reaches
`trip.user-events` at all — which a fake would only have proved to itself.

The scheduler is tested twice over, and deliberately at two levels.
`internal/store/lifecycle_test.go` drives `Store.LifecycleTick` against
Postgres: it seeds a trip whose `end_at` has passed, ticks, and asserts one
`trip.completed` row; ticks again and asserts there is still one; runs two
passes concurrently to make the `SKIP LOCKED` claim against goroutines rather
than against a comment; and forces `completed_event_emitted` on to isolate that
guard from the status check, which in normal operation hides it.
`internal/http/scheduler_api_test.go` does the same two through
`POST /internal/scheduler/tick`. `internal/scheduler` has no database under it
at all — what it covers is when the pass is called and what happens when it
fails, which is the whole of that package.

The discovery tests share one fixture, `testsupport.DiscoveryWorld`: fifty trips
across real Ukrainian places with real coordinates, since the thing under test
is a geographic predicate and made-up points would prove nothing about the one
the acceptance criteria describe. Distances from Ivano-Frankivsk:

```
Ivano-Frankivsk   0 km      Yaremche      ~54 km     Lviv          ~115 km
Halych           ~22 km     Bukovel       ~66 km     Chernivtsi    ~114 km
Kalush           ~27 km     Hoverla       ~86 km     Kyiv          ~470 km
```

So a 50 km radius contains exactly three seeded departures and the near misses
at 54 and 66 km are what make it mean something. Expectations are computed from
the seed list rather than written out as literals wherever the filter has a Go
equivalent: a hand-written list of titles is a second implementation of the
filter that drifts the moment the fixture changes.

The dashboard tests build their own world instead of reusing that fixture. What
they assert is *relationships* — who organizes what, who was approved onto what,
who is still waiting — and none of those can be seeded by `SeedTrips`, because
they are written by the join-request flow rather than by `CreateTrip`. So
`internal/store/dashboard_test.go` builds a dozen trips through the real store
methods and spells the relationships out. Two of its fixtures write a row
directly, and both are marked: a `join_requests` row against a draft, which the
API cannot produce because a draft is unfindable, and a pending request
alongside a `participants` row, which `CreateJoinRequest` refuses. Each exists
to prove the query excludes or collapses a state rather than that nothing has
happened to produce it yet.

Set `TRIP_TEST_DATABASE_URL` to run against an already-running database instead
of starting a container. Be aware that the migration test rolls that database
back and forward again.

## Layout

```
cmd/server      the API process; never runs migrations
cmd/migrate     goose over the embedded migrations, as a separate step
internal/config Load(), the only reader of the environment
internal/auth   JWKS cache, token verification, the caller on the context
internal/domain types, validation, the status state machine — no I/O
internal/events the envelope, the payloads, the outbox relay, the user consumer
internal/scheduler the lifecycle loop: a timer with a batch size attached
internal/store  hand-written SQL over pgx; write paths hold the row lock
internal/users  the projection-first resolver, identity's batch client, its cache
internal/http   router, middleware, handlers, and the one error mapper
migrations/     goose SQL, embedded
```

`internal/http` is `package httpapi`: every file in it also imports `net/http`,
and one of the two would otherwise need an alias throughout.
