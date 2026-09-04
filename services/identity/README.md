# identity

Accounts, authentication and RS256 token issuance. This is the only service
that holds the JWT private key; every other service verifies tokens itself
against `GET /.well-known/jwks.json`.

## Endpoints

| Method | Path | |
| --- | --- | --- |
| `POST` | `/api/auth/register` | 201 → `{user, access_token, refresh_token, expires_in}` |
| `POST` | `/api/auth/login` | 200 → same shape |
| `POST` | `/api/auth/refresh` | 200 → `{access_token, refresh_token, expires_in}` |
| `POST` | `/api/auth/logout` | 204, revokes the presented token's whole chain |
| `GET` | `/api/users/me` | the caller's own profile, email and phone included |
| `PATCH` | `/api/users/me` | `{full_name?, bio?, phone?, photo_url?}` → updated profile |
| `POST` | `/api/users/me/password` | 204, revokes every session except the caller's |
| `GET` | `/api/users/{id}` | someone else's public profile — never email or phone |
| `GET` | `/api/users/{id}/ratings` | the ratings that user received — never who wrote them |
| `GET` | `/api/ratings/pending` | trips the caller still owes ratings on, grouped by trip |
| `POST` | `/api/ratings` | `{trip_id, ratee_id, score, comment?}` → 201 |
| `GET` | `/internal/users?ids=…` | batch resolver for other services; **not** routed through the gateway |
| `GET` | `/.well-known/jwks.json` | the RS256 public key |
| `GET` | `/healthz` | liveness, checks nothing |
| `GET` | `/readyz` | readiness, checks Postgres, the outbox relay and the trip-events consumer |

Public routes reach the service through the gateway on
`http://localhost:8080`, which routes `/api/auth`, `/api/users`,
`/api/ratings` and `/.well-known` here. `/internal` is deliberately absent
from that table — see below.

The generated OpenAPI document lives at `contracts/openapi/identity.yaml`
(`make openapi-identity` to regenerate it). Interactive docs are at
`/docs` on the service port.

## Running it

From the repository root:

```sh
make keys              # once — writes deploy/keys/jwt_private.pem
make up                # brings up postgres, redis, rabbitmq, gateway, identity
make migrate-identity  # migrations are never run on container startup
```

Locally, without a container:

```sh
cd services/identity
uv sync
export IDENTITY_DATABASE_URL=postgresql+asyncpg://identity_user:identity_pass@localhost:5432/identity_db
export REDIS_URL=redis://localhost:6379/0
export RABBITMQ_URL=amqp://togethergo:togethergo@localhost:5672/
export JWT_PRIVATE_KEY_PATH=../../deploy/keys/jwt_private.pem
export JWT_KEY_ID=dev-key-1
export INTERNAL_API_TOKEN=dev-internal-token-change-me

uv run alembic upgrade head
uv run uvicorn app.main:create_app --factory --host 0.0.0.0 --port 8001
```

There is no module-level `app` object — building one would read the
environment as an import side effect. Hence `--factory`.

## Tests

```sh
uv run pytest          # or, from the root: make test-identity
```

Postgres and Redis come up as containers via `testcontainers`, so a working
Docker socket is required. The schema under test is produced by
`alembic upgrade head`, not `metadata.create_all` — a migration that drifts
from the models is a bug the suite should catch rather than one production
finds. The outbox relay is disabled in tests; what the tests assert is that
the row lands in the table inside the right transaction, which is the part
that can actually be wrong.

The trip-events consumer is disabled in the app fixture for the same reason.
`tests/test_ratings.py` hands `trip.completed` envelopes straight to
`RatingEventHandler` — the object the consumer calls for every delivery — so
the transaction, the `processed_events` claim and the writes are exercised
without a broker.

`tests/test_trip_events_consumer.py` covers what that cannot: it starts a
RabbitMQ container loaded with the repository's own
`deploy/rabbitmq/definitions.json` and runs the real consumer against the
real queue. It asserts on how each delivery was *settled* — acked, or nacked
without requeue — rather than on the queue's depth, because a queue counts
only its ready messages and a delivery that was handled and never
acknowledged is invisible to that count. It also asserts the consumer does
not reconnect while the broker is healthy; the first version of this service
churned a new subscription every second and every other test still passed.

## Configuration

Everything comes from the environment through a single `config.load()` that
fails fast. Nothing reads `os.environ` anywhere else.

| Variable | Default | |
| --- | --- | --- |
| `IDENTITY_DATABASE_URL` | — | required, `postgresql+asyncpg://…` |
| `REDIS_URL` | — | required, rate-limit storage |
| `RABBITMQ_URL` | — | required, outbox relay |
| `RABBITMQ_EXCHANGE` | `togethergo.events` | |
| `JWT_PRIVATE_KEY_PATH` | — | required; must be RSA, ≥ 2048 bits |
| `JWT_KEY_ID` | — | required; becomes the JWKS `kid` and the token header `kid` |
| `INTERNAL_API_TOKEN` | — | required, ≥ 16 chars; the `X-Internal-Token` every /internal caller sends |
| `ACCESS_TOKEN_TTL` | `900` | seconds |
| `REFRESH_TOKEN_TTL` | `2592000` | seconds (30 days) |
| `AUTH_RATE_LIMIT_ATTEMPTS` | `10` | per IP per window |
| `AUTH_RATE_LIMIT_WINDOW` | `900` | seconds |
| `OUTBOX_RELAY_ENABLED` | `true` | off in tests |
| `TRIP_EVENTS_CONSUMER_ENABLED` | `true` | off in tests |
| `TRIP_EVENTS_QUEUE` | `identity.trip-events` | declared by the broker, named here |
| `TRIP_EVENTS_PREFETCH` | `10` | the value `contracts/events.md` fixes for every consumer |
| `RATING_SWEEP_ENABLED` | `true` | off in tests |
| `RATING_SWEEP_INTERVAL` | `86400` | seconds; housekeeping only |
| `LOG_LEVEL` / `ENVIRONMENT` | `info` / `local` | |

`JWT_ISSUER` from the root `.env` is **not** read. CLAUDE.md pins the access
token claims to exactly `sub`, `email`, `iat`, `exp`, `jti`, `typ`, and
adding an `iss` would put a claim on the wire that no verifier is specified
to check.

## Layout

```
app/
├── api/            routers — HTTP in, HTTP out, no decisions
├── services/       business logic (auth, passwords, tokens, rate limiting)
├── repositories/   data access, returns models
├── models/         SQLAlchemy tables
├── schemas/        Pydantic request/response models — all input validation
├── events/         envelope, outbox relay, trip-events consumer
├── config.py       the only place the environment is read
├── db.py           engine and session lifecycle
├── deps.py         FastAPI dependencies
├── errors.py       domain errors and the single API error shape
└── main.py         wiring
```

---

## Decisions worth knowing about

### Failure paths are indistinguishable on purpose

`POST /api/auth/login` returns the same status, the same `code` and the same
`message` for an unknown address, a wrong password and a deactivated
account. All three also perform exactly one Argon2id verification — the
unknown-address path verifies against a dummy hash generated at import from a
random secret, so it costs the same ~50 ms as a real one. Without that, the
response time is a working oracle for which addresses are registered.

The `is_active` check happens *after* verification for the same reason: an
early return would make a deactivated account answer measurably faster.

Registration's conflict response says only "The account could not be
created." Whether the address was already taken is not in the body.

### Refresh tokens: rotation, replay, and why logout is different

Every refresh rotates. The presented row is marked revoked and its
`replaced_by` points at the successor, which makes each login session a
linked list of rows.

Presenting a token that is **revoked and has a successor** means someone is
replaying a token that was already spent. Either a thief has a stolen copy,
or the legitimate holder is presenting theirs after a thief spent it first —
there is no way to tell which, so both are treated as a compromise: every
live token for that account is revoked and the caller gets a 401 identical to
any other bad-token 401. The attacker learns that the token stopped working,
not that they were spotted.

Presenting a token that is **revoked with no successor** is different. That
is a session someone ended deliberately — logout, or an earlier reuse sweep —
and replaying it is an ordinary stale-token 401 with no sweep. Collapsing
these two cases is a real bug and an easy one to write: one client refreshing
shortly after logout would look exactly like a replay and would sign the
account out on every other device.

The lookup takes `SELECT … FOR UPDATE`. Two refreshes racing with the same
token serialise on that lock; the first rotates and the second reads the
committed `revoked_at` and reports a replay. Without the lock both could read
`revoked_at IS NULL` and both mint a successor.

Logout revokes the whole rotation chain — a recursive CTE walking
`replaced_by` in both directions, since the presented token is usually the
newest link but need not be. It revokes one chain, not the user's tokens, so
signing out on a laptop leaves the phone signed in. Unknown tokens still
return 204: the endpoint is not an existence oracle either.

### Argon2id runs in a worker thread

Hashing is ~50 ms of CPU. Doing it inline would stall the event loop for
every other in-flight request, so `register` and `login` push it through
`asyncio.to_thread`. The C implementation releases the GIL, so the worker
thread genuinely runs in parallel.

### The rate limiter fails open

Ten attempts per IP per 15 minutes on register and login, as a sliding window
— a sorted set of attempt timestamps in Redis, trimmed, counted and appended
inside one Lua script so the read-modify-write is atomic across replicas. A
fixed-window counter would be simpler and wrong in a specific way: it lets a
caller spend the full budget at 14:59 and the full budget again at 15:00.

If Redis is unreachable the check **allows** the request and logs a warning.
A rate limiter is a control on abuse, not on correctness; losing it should
degrade protection, not take login offline for everyone.

The bucket key is the *rightmost* entry of `X-Forwarded-For`. Traefik appends
the peer it saw, so that entry is the one our own gateway observed and the
only one a client cannot forge — taking the leftmost, which is the usual
reflex, would let any caller reset its own bucket with a made-up header.

### `user.registered` and the outbox

The user row, the outbox row and the first refresh token are written in one
transaction. There is no window in which the account exists and the event
does not; a registration that fails leaves neither.

The relay is an asyncio task in this process, polling every second in batches
of 100 with `FOR UPDATE SKIP LOCKED`, publishing with publisher confirms on,
and marking the confirmed rows published — all inside one transaction. Dying
anywhere before the commit rolls back the marks, releases the locks and
republishes the batch on the next tick. That is where at-least-once delivery
comes from, and why `outbox.id` — generated by the writer — becomes the
envelope's `event_id`: a republished row carries the same id and consumers
deduplicate on it. Published rows are pruned hourly after seven days.

The relay never declares topology. Exchanges, queues and bindings are
imported by the broker at boot from `deploy/rabbitmq/definitions.json`.

The event's `verification_token` is generated per registration and put on the
bus, but **not stored**. Consuming it belongs to the email-verification flow,
which is not built yet and which the given data model has no column for.
Wiring that up means adding a column and an endpoint, not changing the event.

### The internal resolver is batch-only, and unroutable

`GET /internal/users?ids=a,b,c` is how the trip and chat services turn user
ids into a display name, avatar and rating without querying `identity_db`.
Three things about it are contract rather than implementation, and
`tests/test_internal_users.py` holds each of them:

**Batch only, 100 ids maximum.** There is deliberately no single-id variant.
Offering one guarantees it gets called from inside a loop over a trip's
roster, and the resulting N+1 would be nobody's fault in particular. More
than 100 ids is a 400 rather than a silent truncation, because a caller that
got 100 of the 150 users it asked for would render placeholders for the rest
and never find out why. The limit counts the ids as sent, before duplicates
are collapsed.

**Unknown ids are omitted, not an error.** A trip whose member deleted their
account still has to render; the caller shows a placeholder for the id it did
not get back. Deactivated accounts *are* returned — `is_active` governs
whether someone can sign in, not whether they existed.

**`Cache-Control: private, max-age=60`.** A documented TTL every consumer
aligns its own cache with, so a changed display name is stale platform-wide
for a bounded and known interval instead of for however long each service
happened to pick.

The route is not in `deploy/traefik/dynamic.yml`, so a request to
`http://localhost:8080/internal/users` is a 404 from Traefik that never
reaches this application — the endpoint is unreachable from outside, not
merely unauthorised. `tests/test_gateway_routes.py` parses that file and
fails if any router's rule would match `/internal`, and also fails if a new
`/internal` route is added without the exclusion still holding. The
`X-Internal-Token` check is the second line of defence behind that, because
in Compose every service shares a network with every other.

### `age`, and why `birth_date` never leaves the account holder

A public profile carries `age`, computed from `birth_date` on every read.
`PublicProfile` has no `birth_date` field at all, so there is no code path
that can fill one in — the same reason it has no `email` and no `phone`.
Storing an age instead of computing it would be wrong for a fraction of its
holders every single day.

### A PATCH publishes only when the projection cares

`user.profile_updated` feeds the trip service's local copy of a user's
display name and avatar. So a PATCH publishes when `full_name` or
`photo_url` actually changes value, and stays silent for `bio` and `phone` —
an event for a bio edit is one every consumer has to receive and then decide
to ignore. "Actually changes" is compared against the stored value, not
merely against the key being present: setting a name to the name it already
had has changed nothing.

The payload is still the *full* current profile, `bio` included, because
`contracts/events.md` specifies it as a snapshot rather than a delta. The row
and the event carry the same `updated_at` — set explicitly rather than left
to the column's `onupdate` — because consumers compare that timestamp to
discard out-of-order updates, and a row and an event that disagreed about it
would make that comparison a coin flip.

Omitting a field in a PATCH leaves it alone; sending it as `null` clears it.
The two are told apart with Pydantic's `model_fields_set`, not by filtering
out `None`. `full_name` is `NOT NULL`, so an explicit null there is a 422 and
not a clear.

### Changing a password ends every session but yours

The endpoint re-verifies `current_password` — that is the whole point of the
`current_password` field, and it means a stolen access token alone cannot
change a password. It then revokes every live refresh token for the account
*except the caller's own*, because someone changing their password is often
doing it precisely because they think an attacker holds one, and logging them
out of the tab they just used is not a security property.

Identifying "the caller's own" needs a bridge, because an access token has no
handle on the refresh token that came with it and CLAUDE.md pins the claim
set to exactly `sub`, `email`, `iat`, `exp`, `jti`, `typ` — adding a session
claim was not available. So `refresh_tokens.access_token_jti` records the
`jti` of the access token minted alongside each row, and the endpoint spares
the whole rotation *chain* that row belongs to. Sparing the row alone would
not do: an access token is valid for fifteen minutes, so a client that
refreshed shortly before changing its password would be asking us to spare a
row that has already been rotated away, and would log itself out.

An unmatched `jti` revokes everything. That is the safe direction to fail in.

What the endpoint cannot do is invalidate access tokens already in the wild;
they are stateless and stay valid for up to fifteen minutes. Fixing that
means a revocation list every service checks on every request, which is a
real cost to weigh against a fifteen-minute window and is not part of the
MVP.

Wrong `current_password` is a **403**, not a 401. The access token is fine
and the caller is authenticated; a 401 would tell every client library to
throw the session away and refresh, which is the wrong reaction to a typo in
a form field.

### `/readyz` reports the broker, and that has a cost

CLAUDE.md says readiness checks the database "and broker where relevant", and
this service publishes, so it reports both — meaning a RabbitMQ outage takes
identity out of the gateway's rotation and logins stop, even though login
itself needs no broker. That is the stated convention followed as written,
and it is the kind of coupling worth revisiting if it ever bites: the
narrower rule would be to fail readiness only on the database and alert on
the relay separately.

### citext

`users.email` is `citext`, so `Alex@Example.com` and `alex@example.com` are
one account and the unique index enforces it. The alternative — a functional
unique index on `lower(email)` — works too but requires every query to
remember to match the same way. The address is stored as the user typed it.

### Ratings live here, not in the trip service

A rating is a property of a *user*. `GET /api/users/{id}` has to show the
aggregate, and it must be able to do that without touching trips — so the
rows, the window and the aggregate all live in `identity_db`.

That choice is what makes the eligibility rule implementable. A submission is
authorised by a `pending_ratings` row and by nothing else: identity never
calls the trip service to ask whether two people travelled together. The
roster arrives once, in the `trip.completed` payload, which is the one
deliberately fat payload in the catalogue and is fat for exactly this reason.
The rating window therefore keeps working while trip is down.

### The aggregate is a sum and a count, never a stored average

`user_rating` holds `rating_sum` and `rating_count`. `rating_avg` is
`round(rating_sum::numeric / NULLIF(rating_count, 0), 2)`, computed by
Postgres on the way out and stored nowhere.

An average cannot be incremented. Storing one would force every submission to
recompute it from rows it would first have to read — or to keep a count
beside it anyway, at which point the average is redundant. A sum is a single
`+= score`, and the UPSERT that applies it is what makes two people rating
the same traveller at the same moment safe: `rating_sum = user_rating.rating_sum
+ excluded.rating_sum` is evaluated by the database while it holds the row,
so there is no read-modify-write window to lose a rating in.

Nothing recomputes the aggregate on a schedule. It is maintained by the
increment, in the same transaction as the rating that caused it.

`NULLIF` is why an unrated account reads `null` and not `0.00`: "not rated
yet" and "rated zero" are different facts, and only one of them is true.

### Ratings are anonymous to the ratee

`GET /api/users/{id}/ratings` returns the score, the comment, the date and
the trip title. It does not return `rater_id`, and the query behind it does
not read that column — anonymity is a property of the projection, not of a
`del` in a serialiser that someone can forget.

The endpoint also takes **no sort and no filter parameter**, only `limit` and
an opaque `cursor`. That is the less obvious half of the rule. A ratee who
could sort by score, or filter by trip or by date range, could intersect the
result with a roster they can already see and work out who said what. One
fixed order and no predicates leaves them a page and nothing else.

### One transaction per submission, and the 409 comes before the 403

Inserting the rating, completing the pending row, incrementing the aggregate
and staging `user.rating_updated` are one transaction (CLAUDE.md rule 4). A
rating that committed without its event, or an event for a rating that rolled
back, are both states the outbox exists to rule out.

The order of the two refusals is deliberate. A client re-sending a submission
it already made has a *completed* pending row, so an eligibility-first check
would answer `not_eligible` and send the user looking for a window that closed
because they used it. Checking for the existing rating first answers
`rating_already_submitted`, which is what actually happened.

`not_eligible` is one answer for every reason — the trip never completed, the
fourteen days ran out, the two never travelled together, the caller is rating
themselves. Distinguishing them would turn the endpoint into an oracle for
"was X on trip Y".

### The nightly sweep is housekeeping, not correctness

Every read of `pending_ratings` filters on `expires_at`, so a window is
invisible and unusable the moment it lapses, whether or not anything swept
it. `PendingRatingSweeper` sets `completed_at` on lapsed rows once a day
purely so the table and its partial index stay the size of the work actually
outstanding, rather than growing by `n * (n - 1)` rows per completed trip
forever. A sweep that never ran would change no answer this service gives.

### Identity is a consumer now, too

`identity.trip-events` carries `trip.completed`, and the consumer is an
asyncio task in this process — same shape as the Go services' consumers, for
the same reasons. It never declares topology, it acks manually, and it never
requeues: a `requeue=true` puts a poison message back at the head of the
queue and the consumer spins on it at full speed. Retries happen in-process
on the contract's 200/400/800 ms ladder with ±20 % jitter, and what survives
that is dead-lettered to `identity.trip-events.dlq`.

Duplicates are absorbed by `processed_events`, claimed with
`INSERT … ON CONFLICT DO NOTHING` in the same transaction as the work
(CLAUDE.md rule 5). An unknown `event_type` is marked processed and acked,
not dead-lettered: it is a message for a newer build, and a DLQ full of
messages nobody will ever act on is worse than useless.

`ON CONFLICT DO NOTHING` on `pending_ratings` backs that up. A replay from the
DLQ under a fresh `event_id` gets past the idempotency claim, and the unique
key on `(trip_id, rater_id, ratee_id)` is what keeps it from doubling
everyone's list.

### No shared library

`app/events/envelope.py` duplicates a struct that also exists in Go in the
trip and chat services. That is deliberate (CLAUDE.md rule 7):
`contracts/events.md` is the contract both sides answer to, and a shared
package would couple their deploy cycles for the sake of thirty lines.

## Not built yet

Email verification, and anything trip-related beyond the rating window.
