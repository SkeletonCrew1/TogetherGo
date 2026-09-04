# TogetherGo
 
A platform for organizing and finding shared trips (hikes, city trips, food tourism). Users create trips with a geographic route, other users request to join, the organizer approves, approved participants get a group chat, and after the trip completes everyone rates their co-travelers.
 
## Repository layout
 
```
/
├── CLAUDE.md
├── docker-compose.yml
├── Makefile
├── .env.example
├── contracts/
│   ├── events.md            # canonical event catalogue
│   └── openapi/             # one OpenAPI file per public service
├── deploy/
│   ├── traefik/             # gateway static + dynamic config
│   ├── rabbitmq/            # definitions.json: exchanges, queues, bindings
│   └── postgres/init/       # DB + role bootstrap SQL
├── services/
│   ├── identity/            # Python 3.12, FastAPI
│   ├── trip/                # Go 1.23
│   ├── chat/                # Go 1.23
│   └── notification/        # Python 3.12
└── web/                     # React SPA (added in a later stage)
```
 
Monorepo. Each service is independently buildable and has its own Dockerfile, migrations, tests and README.
 
## Architecture rules — these are not negotiable
 
1. **Database per service.** One Postgres container, four databases (`identity_db`, `trip_db`, `chat_db`, `notification_db`), four roles. Each role has privileges on its own database only. A service must never connect to another service's database, and there is never a SQL join across service boundaries. If you find yourself wanting one, the fix is either an event projection or an internal HTTP call.
2. **REST for synchronous request/response. WebSockets only for chat.** No WebSocket usage anywhere else, no gRPC in the MVP.
3. **Asynchronous cross-service communication goes through RabbitMQ**, using the event catalogue in `contracts/events.md`.
4. **Every event publish uses the transactional outbox pattern.** The domain write and the outbox insert happen in the same database transaction; a separate relay publishes and marks rows as published. Never publish to RabbitMQ inline inside a request handler.
5. **Every event consumer is idempotent**, backed by a `processed_events (event_id uuid primary key, processed_at timestamptz)` table in the consumer's own database. Check-then-process inside one transaction.
6. **Services validate their own JWTs** against the identity service's JWKS. Do not trust gateway-injected headers as the sole source of identity — in Compose all services are reachable on the same network.
7. **No shared library across languages.** Duplicating a 30-line event envelope struct in Go and Python is correct here; a shared package would couple deploy cycles.
## Infrastructure (docker-compose)
 
| Component | Image | Host port |
| --- | --- | --- |
| gateway | `traefik:v3.1` | 8080 (API), 8081 (dashboard) |
| postgres | `postgis/postgis:16-3.4` | 5432 |
| redis | `redis:7-alpine` | 6379 |
| rabbitmq | `rabbitmq:3.13-management` | 5672, 15672 |
| mailhog | `mailhog/mailhog` | 1025 (SMTP), 8025 (UI) |
| identity | built | 8001 |
| trip | built | 8002 |
| chat | built | 8003 |
| notification | built | 8004 (health only) |
 
All routing for clients goes through `http://localhost:8080`. Direct service ports are exposed for debugging only.
 
Route prefixes at the gateway:
 
```
/api/auth/*      → identity
/api/users/*     → identity
/api/ratings/*   → identity
/api/trips/*     → trip
/api/my/*        → trip
/api/chat/*      → chat
/ws/chat/*       → chat   (websocket)
```
 
## Authentication
 
- Identity service signs JWTs with **RS256**. The private key lives only in identity. The public key is served at `GET /.well-known/jwks.json`.
- Other services fetch and cache the JWKS (refresh every 10 minutes, retry on unknown `kid`).
- Access token TTL 15 minutes. Claims: `sub` (user uuid), `email`, `iat`, `exp`, `jti`, `typ: "access"`.
- Refresh token TTL 30 days, opaque random 32 bytes, stored **hashed** (SHA-256) in identity's database, rotated on every use. Reuse of an already-rotated token revokes the whole chain.
- Internal service-to-service calls carry `X-Internal-Token` matching `INTERNAL_API_TOKEN` from the environment. Internal routes are never exposed through the gateway.
## Event envelope
 
Every message published to RabbitMQ is JSON with this exact envelope:
 
```json
{
  "event_id": "uuid v4",
  "event_type": "join_request.approved",
  "event_version": 1,
  "occurred_at": "2026-08-27T12:00:00Z",
  "aggregate_id": "uuid of the trip/user this concerns",
  "payload": { }
}
```
 
Exchange: `togethergo.events`, type `topic`, durable. Routing key = `event_type`. Dead-letter exchange: `togethergo.dlx` with per-queue DLQs. Messages are published persistent.
 
## Conventions
 
- All IDs are UUID v4. All timestamps are `timestamptz`, stored and transmitted in UTC as RFC 3339.
- All coordinates use SRID 4326 and the PostGIS `geography` type.
- API errors use a single shape: `{"error": {"code": "trip_full", "message": "...", "details": {...}}}`. HTTP status carries the class, `code` carries the specific reason.
- List endpoints use **keyset pagination**, never `OFFSET`. Response shape: `{"items": [...], "next_cursor": "opaque base64 or null"}`.
- Migrations: `goose` for Go services, `alembic` for Python services. Migrations run as a separate step, never automatically on service startup.
- Structured JSON logs to stdout with `service`, `level`, `msg`, `request_id`, and `user_id` when known. Never log tokens, password hashes, or full request bodies.
- Health endpoints on every service: `GET /healthz` (liveness, no dependency checks) and `GET /readyz` (checks DB, and broker where relevant).
## Go conventions (`trip`, `chat`)
 
- Go 1.23, standard project layout: `cmd/server/main.go`, `internal/{http,domain,store,events,config}`.
- Router: `chi/v5`. Database: `pgx/v5` with `pgxpool`. **No ORM.** Hand-written SQL in a `store` package that returns domain types.
- Config from environment via a single `config.Load()` that fails fast on missing required values.
- Errors: wrap with `fmt.Errorf("...: %w", err)`. Domain errors are sentinel values in `internal/domain` mapped to HTTP codes in one place.
- `context.Context` threaded through every store and service call.
- Tests: `testify/require` plus `testcontainers-go` for store tests against real Postgres.
## Python conventions (`identity`, `notification`)
 
- Python 3.12, dependencies managed with `uv` and `pyproject.toml`.
- FastAPI with Pydantic v2 models. Every request body and query parameter set is a Pydantic model with explicit constraints.
- SQLAlchemy 2.0 async with `asyncpg`. Alembic for migrations.
- Layering: `api/` (routers) → `services/` (business logic) → `repositories/` (data access) → `models/` (SQLAlchemy). Routers contain no business logic.
- Passwords hashed with Argon2id (`argon2-cffi`).
- Tests: `pytest` + `pytest-asyncio` + `httpx.AsyncClient`, Postgres via `testcontainers`.
## What is explicitly out of scope for the MVP
 
Kubernetes/EKS, Consul, Terraform, Prometheus/Grafana, Elasticsearch, separate Postgres instances per service, S3 photo uploads, push notifications, payments. Do not add these, do not scaffold for them, do not write comments referencing them. They are a later phase.
