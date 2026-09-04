# TogetherGo

A platform for organizing and finding shared trips — hikes, city trips, food
tourism. A user creates a trip with a geographic route, other users request to
join, the organizer approves, approved participants get a group chat, and once
the trip completes everyone rates their co-travellers.

This repository is a monorepo of four independently buildable services plus a
React SPA. The frontend runs *inside* the stack and is served by the gateway on
the same origin as the API, so **`make up` is the only command needed to run
the whole system** — there is no separate dev server to start.

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
make migrate              # apply every service's migrations
```

Then open **http://localhost:8080**. That is the app *and* the API: the SPA is
served by Traefik from the same origin as `/api`, so there is nothing else to
start — no `npm install`, no `npm run dev`, no second port.

`make up` returns only once every container reports healthy. First run pulls
images, installs the frontend's dependencies and initialises Postgres, so
expect a few minutes.

Editing anything under `web/src` reloads the open page: the source is
bind-mounted into the container and Vite's HMR socket rides the same gateway
the app does.

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
| web | built | *(none)* | The React SPA, served through the gateway |

Client traffic goes through `http://localhost:8080`. The direct service ports
are for debugging only.

`web` is the one service with **no host port at all**, and that is deliberate
rather than an omission. It listens on 5173 inside the container and Traefik
dials it by name; publishing that port would give the SPA a second origin, and
a second origin is exactly what produced this project's CORS configuration and
the chat service's WebSocket `Origin` rejection. One origin, one class of bug
gone.

`chat` is the one service that runs more than one container, which is why it has
no fixed host port and no `container_name`: `docker compose up --scale chat=2`
(or `make scale-chat`) gives the replicas 8003 and 8004, and the Redis fan-out is
what makes them one chat rather than two. The gateway load-balances between them
with no sticky sessions.

## Make targets

| Target | Does |
| --- | --- |
| `make up` | Start everything — infrastructure, services and the SPA — and wait for healthy |
| `make down` | Stop and remove containers, keep volumes |
| `make restart` | `down` then `up` |
| `make ps` | Container status and health |
| `make logs` | Follow all logs (`make logs SERVICE=postgres` to narrow) |
| `make logs-web` | Follow the frontend's logs (the Vite dev server) |
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
   browser ─8080──▶  │ traefik (gateway)│ ──── /  (everything else) ───▶ web
                     └────────┬─────────┘                              (SPA)
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

The table ends with a catch-all: anything that is not `/api`, `/ws` or
`/.well-known` goes to the SPA. It carries an explicit low `priority`, so the
API routers always win regardless of how long their rules happen to be — the
alternative is a page that loads while every API call comes back as
`index.html`.

Routes that are *not* in that file are unreachable from outside, which is a
security boundary and not just tidiness: `/internal/*` is absent, so a request
to `http://localhost:8080/internal/users` is a 404 from Traefik that never
reaches the application. The catch-all would otherwise have swallowed it, which
is why it is written `PathPrefix(`/`) && !PathPrefix(`/internal`)` — the 404
comes from the gateway, not from the SPA answering 200 with an HTML page.

## The frontend

`web/` is a React SPA, and it runs as a container in this stack like everything
else. It used to be a Vite dev server the developer started by hand on
`localhost:5173` while the API sat behind Traefik on `8080`, and that split is
the direct cause of two bugs this project has already paid for: the CORS
configuration, and the WebSocket `Origin` rejection in the chat service. Behind
one gateway there is one origin, and neither problem exists to be configured
around.

What that buys, concretely:

- The API client's base URL is the empty string. `fetch('/api/trips')` is
  already correct, so there is no cross-origin request to grant.
- The chat socket derives its host from `window.location` and its scheme from
  `location.protocol`, so it follows the page.
- `CHAT_ALLOWED_ORIGINS` is `localhost:8080` — one host, the real one, rather
  than a wildcard hiding the fact that the frontend moved.
- Hot reload still works. `./web` is bind-mounted into the container and the
  HMR WebSocket is proxied by the same gateway as everything else.

Two images:

| File | For | Notes |
| --- | --- | --- |
| `web/Dockerfile.dev` | development | The Vite dev server. What Compose runs. |
| `web/Dockerfile` | production | Multi-stage: `npm run build`, then nginx over the built `dist/`. Not used by Compose. |

The production image takes `VITE_API_BASE_URL` and `VITE_WS_BASE_URL` as
**build arguments**, not runtime environment variables, because Vite inlines
them into the bundle at build time. An image built with one value cannot be
repointed by setting an env var on the container — the value is in the
JavaScript the browser downloads. Repointing means rebuilding:

```bash
docker build -f web/Dockerfile --build-arg VITE_API_BASE_URL=https://api.example.com web/
```

Both default to empty, which is what makes the bundle same-origin — the right
default for the deployment this project has.

## Troubleshooting

**The page loads, but every API call 404s (or returns HTML).** A Traefik
routing problem, and almost always priority. The SPA's router matches
`PathPrefix(`/`)`, so it matches `/api/trips` too; only its explicit low
`priority` in `deploy/traefik/dynamic.yml` keeps the API routers ahead of it.
Check which router actually won:

```bash
curl -s http://localhost:8080/api/trips | head -c 100   # JSON error envelope, not <!doctype html>
open http://localhost:8081/api/http/routers             # the loaded table, with priorities
```

If the table on the dashboard does not match the file, the gateway has not
re-read it — the file provider watches a bind mount and Docker Desktop for
macOS does not reliably deliver that inotify event. `docker compose restart
gateway`.

**The page loads, but edits do not hot reload.** The container is not seeing
the writes. Filesystem events do not propagate reliably through Docker
Desktop's virtualisation layer on macOS, which is why `server.watch.usePolling`
is `true` in `web/vite.config.ts`; if it has been turned off, this is the
symptom. Watch for the update as you save:

```bash
make logs-web    # a healthy save prints an hmr update line
```

A dead HMR socket in the browser console is a different fault with the same
symptom: the page is served from `8080` but the HMR client defaults to its own
port, so without `server.hmr.clientPort: 8080` it dials `ws://localhost:5173`
and fails silently. The initial page load works either way, which is what makes
this one confusing.

**The chat WebSocket handshake returns 403.** The `Origin` allowlist, not auth
— and it reads exactly like an auth failure, which has already cost time here
once. The browser now arrives from `localhost:8080`; `CHAT_ALLOWED_ORIGINS`
must say so.

```bash
docker compose exec chat printenv CHAT_ALLOWED_ORIGINS   # expect localhost:8080,127.0.0.1:8080
docker compose logs chat | grep '"status":403'
```

The tell is the status: a rejected *origin* is a 403 on the HTTP handshake, so
the socket never opens. A rejected *ticket* is a 101 followed by a close with
code 4401 — the connection opens and then closes, deliberately, so the browser
can tell the two apart. If you see 101 and then 4401, the origin is fine and
the problem really is the ticket.

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
    ├── Dockerfile.dev       Vite dev server — what Compose runs
    ├── Dockerfile           multi-stage production build, nginx in front
    └── nginx.conf           SPA fallback, so deep links survive a refresh
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
