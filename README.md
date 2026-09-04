# TogetherGo

A platform for organizing and finding shared trips — hikes, city trips, food
tourism. A user creates a trip with a geographic route, other users request to
join, the organizer approves, approved participants get a group chat, and once
the trip completes everyone rates their co-travellers.

This repository is a monorepo of four independently buildable services plus a
React SPA. **At this stage only the local infrastructure exists** — no service
code yet. `make up` brings up a healthy backing environment to build against.

## Prerequisites

| Tool | Version | Notes |
| --- | --- | --- |
| Docker Engine | 24+ | Docker Desktop on macOS is fine. |
| Docker Compose | v2 | Invoked as `docker compose`, not `docker-compose`. |
| GNU Make | 3.81+ | Ships with macOS; `build-essential` on Debian/Ubuntu. |
| OpenSSL | 1.1+ or 3.x | Used by `make keys`. |

Nothing else is needed to run the infrastructure. Go 1.23 and Python 3.12 come
in when the services themselves are built.

> On Apple Silicon, `postgis/postgis:16-3.4` and `mailhog/mailhog` are amd64
> images and run under emulation. They work; Postgres is just slower to reach
> healthy on first start.

## Quickstart

```bash
make keys                 # generate the RS256 keypair into deploy/keys/
cp .env.example .env      # local defaults, safe to use as-is
make up                   # start everything and wait for healthy
make ps                   # confirm
```

`make up` returns only once every container reports healthy. First run pulls
images and initialises Postgres, so expect a minute or two.

Check the environment is really working:

```bash
# PostGIS is enabled in trip_db
docker compose exec postgres psql -U trip_user -d trip_db -c 'SELECT postgis_version()'

# ...and cross-service database access is refused, loudly
docker compose exec postgres psql -U trip_user -d identity_db
# FATAL: permission denied for database "identity_db"
```

Tear down with `make down` (keeps data) or `make clean` (deletes volumes, which
also makes the next `make up` re-run the database bootstrap).

## Ports

| Component | Image | Host port | What it is |
| --- | --- | --- | --- |
| gateway | `traefik:v3.1` | 8080 | Public API — all client traffic |
| gateway (dashboard) | `traefik:v3.1` | 8081 | Traefik dashboard, `/ping` |
| postgres | `postgis/postgis:16-3.4` | 5432 | Four databases, one per service |
| redis | `redis:7-alpine` | 6379 | Trip cache; chat tickets, presence, rate limit and fan-out |
| rabbitmq | `rabbitmq:3.13-management` | 5672 | AMQP |
| rabbitmq (management) | `rabbitmq:3.13-management` | 15672 | Web UI (`togethergo` / `togethergo`) |
| mailhog | `mailhog/mailhog` | 1025 | SMTP sink |
| mailhog (UI) | `mailhog/mailhog` | 8025 | Captured mail |
| identity | built | 8001 | Accounts, auth, profiles, JWKS |
| trip | built | 8002 | Trips, routes, participants, lifecycle |
| chat | built | 8003+ | Group chat: WebSockets, room projection, Redis fan-out |
| notification | built | 8004 | *not yet built*, health only |

Client traffic goes through `http://localhost:8080`. The direct service ports
are for debugging only.

`chat` is the one service that runs more than one container, which is why it has
no fixed host port and no `container_name`: `docker compose up --scale chat=2`
(or `make scale-chat`) gives the replicas 8003 and 8004, and the Redis fan-out is
what makes them one chat rather than two. The gateway load-balances between them
with no sticky sessions.

## Make targets

| Target | Does |
| --- | --- |
| `make up` | Start the stack, wait for healthy |
| `make down` | Stop and remove containers, keep volumes |
| `make restart` | `down` then `up` |
| `make ps` | Container status and health |
| `make logs` | Follow all logs (`make logs SERVICE=postgres` to narrow) |
| `make clean` | Stop and **delete volumes** |
| `make keys` | Generate the RS256 keypair into `deploy/keys/` |
| `make psql-identity` | psql into `identity_db` as `identity_user` |
| `make psql-trip` | psql into `trip_db` as `trip_user` |
| `make psql-chat` | psql into `chat_db` as `chat_user` |
| `make psql-notification` | psql into `notification_db` as `notification_user` |
| `make rabbitmq-ui` | Open the RabbitMQ management UI |
| `make rabbitmq-topology` | Print the declared exchanges, queues and bindings |
| `make rabbitmq-import` | Re-apply `deploy/rabbitmq/definitions.json` to a running broker |
| `make mailhog-ui` | Open the MailHog UI |
| `make migrate` | Run every service's migrations |
| `make migrate-identity` | Apply `identity_db` migrations (alembic) |
| `make migrate-trip` | Apply `trip_db` migrations (goose) |
| `make migrate-trip-down` | Roll back the most recent `trip_db` migration |
| `make migrate-chat` | Apply `chat_db` migrations (goose) |
| `make migrate-chat-down` | Roll back the most recent `chat_db` migration |
| `make test-identity` | Run the identity test suite |
| `make test-trip` | Run the trip test suite |
| `make test-trip-race` | The join-request capacity invariant under `-race`, ten times over |
| `make test-chat` | Run the chat test suite |
| `make test-chat-race` | The socket and fan-out tests under `-race` |
| `make scale-chat` | Two chat replicas behind the gateway (`make scale-chat N=3`) |
| `make lint-trip` | `go vet` and a gofmt check on the trip service |
| `make lint-chat` | `go vet` and a gofmt check on the chat service |

`make keys` will not overwrite an existing private key; delete
`deploy/keys/jwt_private.pem` first if you want to rotate.

## Architecture

Four services, each owning its data and talking to the others two ways: REST
for synchronous request/response, RabbitMQ for everything asynchronous.

```
                     ┌──────────────────┐
   client ──8080──▶  │ traefik (gateway)│
                     └────────┬─────────┘
          /api/auth,users,ratings │ /api/trips,my │ /api/chat,/ws/chat
                     ┌──────────┴───┬─────────────┴──┐
                     ▼              ▼                ▼
                ┌─────────┐   ┌─────────┐      ┌─────────┐    ┌──────────────┐
                │identity │   │  trip   │      │  chat   │    │ notification │
                │ FastAPI │   │   Go    │      │   Go    │    │   FastAPI    │
                └────┬────┘   └────┬────┘      └────┬────┘    └──────┬───────┘
                     │             │                │                │
                identity_db     trip_db          chat_db      notification_db
                     │             │                │                │
                     └─────────────┴───── RabbitMQ ─┴────────────────┘
                                    togethergo.events (topic)
```

**Database per service.** One Postgres container, four databases, four roles.
Each role can connect to exactly one database — `deploy/postgres/init/01-databases.sql`
revokes `CONNECT` from `PUBLIC` and from the other three roles, so a stray DSN
fails at connection time instead of quietly working. There is no SQL join
across a service boundary; when a service needs another's data, the answer is
an event projection or an internal HTTP call. PostGIS is enabled in `trip_db`
only, since it is the only database holding geometry.

**Events.** Every asynchronous message goes to the durable topic exchange
`togethergo.events`, routing key equal to the event type, with a dead-letter
exchange `togethergo.dlx`. Publishes go through a transactional outbox — the
domain write and the outbox insert share a transaction, and a relay publishes
separately — so delivery is at-least-once and every consumer is idempotent via
a `processed_events` table in its own database. The full catalogue, with
payload schemas, delivery semantics and the outbox contract, is in
[`contracts/events.md`](contracts/events.md).

The topology itself is not declared by application code. The broker imports
[`deploy/rabbitmq/definitions.json`](deploy/rabbitmq/definitions.json) at boot,
so all four consumer queues, their DLQs and every binding exist before the first
service connects — rather than depending on whichever service happens to start
first.

**Auth.** Identity signs RS256 JWTs and publishes its public key at
`/.well-known/jwks.json`. Every other service validates tokens itself against
that JWKS rather than trusting gateway-injected headers, because in Compose all
services share a network and are reachable directly. Access tokens last 15
minutes; refresh tokens are opaque, stored hashed, and rotated on every use.
Service-to-service calls carry `X-Internal-Token`, and internal routes are never
exposed through the gateway.

**Gateway.** Traefik with the *file* provider rather than Docker labels, so the
whole routing table stays readable in one place —
[`deploy/traefik/dynamic.yml`](deploy/traefik/dynamic.yml). Traefik watches the
file, so adding a route does not require restarting the gateway — though on
Docker Desktop for macOS the inotify event does not always cross the bind
mount, so `docker compose restart gateway` after editing is the reliable move.

Routes that are *not* in that file are unreachable from outside, which is a
security boundary and not just tidiness: `/internal/*` is absent, so a request
to `http://localhost:8080/internal/users` is a 404 from Traefik that never
reaches the application.

## Layout

```
/
├── docker-compose.yml       infrastructure only, for now
├── Makefile
├── .env.example
├── contracts/
│   ├── events.md            canonical event catalogue
│   └── openapi/             one OpenAPI file per public service
├── deploy/
│   ├── traefik/             gateway static + dynamic config
│   ├── rabbitmq/            exchanges, queues and bindings, loaded at boot
│   ├── postgres/init/       DB + role bootstrap SQL
│   └── keys/                RS256 keypair (gitignored)
├── services/
│   ├── identity/            Python 3.12, FastAPI
│   ├── trip/                Go 1.23 — trips, routes, lifecycle
│   ├── chat/                Go 1.23 — websockets, rooms, fan-out
│   └── notification/        Python 3.12
└── web/                     React SPA
```

Each service is independently buildable and will carry its own Dockerfile,
migrations, tests and README. There is no shared library across languages: the
event envelope is duplicated in Go and Python on purpose, because a shared
package would couple deploy cycles for the sake of thirty lines.

## Configuration

Everything is environment-driven; `.env.example` documents every variable the
stack will eventually need, grouped by service. Copy it to `.env` and edit if
you need different ports. The compose file fails fast on missing required
values rather than falling back to defaults for anything security-relevant.

`deploy/keys/` and `.env` are gitignored. The private JWT key belongs only to
the identity service.
