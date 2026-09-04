"""Application wiring.

`create_app` builds everything the process needs and hangs it off
`app.state`; the lifespan owns the pieces that have to be opened and closed.
Nothing in this module makes a decision about authentication — it only
assembles.

There is deliberately no module-level `app`. Building one at import time
would read the environment as a side effect of importing, which makes the
module unimportable in a test that has not set it up yet. Run it as

    uvicorn app.main:create_app --factory --host 0.0.0.0 --port 8001
"""

from __future__ import annotations

import logging
import time
import uuid
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

from fastapi import FastAPI, Request
from redis.asyncio import Redis

from app import logging as app_logging
from app.api import (
    auth_router,
    health_router,
    internal_router,
    jwks_router,
    ratings_router,
    user_ratings_router,
    users_router,
)
from app.config import Settings, load
from app.db import create_engine, create_sessionmaker
from app.errors import install_handlers
from app.events.consumer import TripEventsConsumer
from app.events.relay import OutboxRelay
from app.services.rate_limit import RateLimiter
from app.services.rating_service import RatingEventHandler
from app.services.rating_sweeper import PendingRatingSweeper
from app.services.tokens import load_issuer

logger = logging.getLogger(__name__)

# Paths that would otherwise fill the log with probe traffic.
_QUIET_PATHS = frozenset({"/healthz", "/readyz"})


@asynccontextmanager
async def lifespan(app: FastAPI) -> AsyncIterator[None]:
    settings: Settings = app.state.settings

    # Loaded before anything else opens a socket: a process that cannot sign
    # tokens should fail at boot, not at the first login.
    app.state.issuer = load_issuer(
        settings.jwt_private_key_path,
        key_id=settings.jwt_key_id,
        access_token_ttl=settings.access_token_ttl,
    )

    engine = create_engine(settings.database_url)
    app.state.engine = engine
    app.state.sessionmaker = create_sessionmaker(engine)

    redis = Redis.from_url(settings.redis_url, decode_responses=False)
    app.state.redis = redis
    app.state.rate_limiter = RateLimiter(
        redis,
        limit=settings.auth_rate_limit_attempts,
        window_seconds=settings.auth_rate_limit_window,
    )

    relay: OutboxRelay | None = None
    if settings.outbox_relay_enabled:
        relay = OutboxRelay(
            sessionmaker=app.state.sessionmaker,
            amqp_url=settings.rabbitmq_url,
            exchange_name=settings.rabbitmq_exchange,
            poll_interval=settings.outbox_poll_interval,
            batch_size=settings.outbox_batch_size,
            retention_days=settings.outbox_retention_days,
        )
        await relay.start()
    app.state.relay = relay

    # Identity is a consumer as well as a publisher as of the rating system:
    # `trip.completed` carries the roster that opens a rating window, and it is
    # the only inbound event this service takes.
    consumer: TripEventsConsumer | None = None
    if settings.trip_events_consumer_enabled:
        consumer = TripEventsConsumer(
            handler=RatingEventHandler(app.state.sessionmaker),
            amqp_url=settings.rabbitmq_url,
            queue=settings.trip_events_queue,
            prefetch=settings.trip_events_prefetch,
        )
        await consumer.start()
    app.state.consumer = consumer

    sweeper: PendingRatingSweeper | None = None
    if settings.rating_sweep_enabled:
        sweeper = PendingRatingSweeper(
            sessionmaker=app.state.sessionmaker,
            interval=settings.rating_sweep_interval,
        )
        await sweeper.start()
    app.state.rating_sweeper = sweeper

    logger.info("identity service started", extra={"environment": settings.environment})
    try:
        yield
    finally:
        # Stopped in the reverse of the order they were started, and before the
        # engine is disposed: both hold sessions from it.
        if sweeper is not None:
            await sweeper.stop()
        if consumer is not None:
            await consumer.stop()
        if relay is not None:
            await relay.stop()
        await redis.aclose()
        await engine.dispose()
        logger.info("identity service stopped")


def create_app(settings: Settings | None = None) -> FastAPI:
    settings = settings or load()
    app_logging.configure(settings.log_level)

    app = FastAPI(
        title="TogetherGo identity",
        description=(
            "Accounts, authentication and RS256 token issuance. "
            "The private signing key exists only in this service; every other "
            "service verifies tokens against /.well-known/jwks.json."
        ),
        version="0.1.0",
        lifespan=lifespan,
        # The gateway strips nothing, so the service owns its full paths.
        docs_url="/docs",
        redoc_url=None,
        openapi_url="/openapi.json",
    )
    app.state.settings = settings

    install_handlers(app)

    @app.middleware("http")
    async def request_context(request: Request, call_next):
        # Honour an inbound X-Request-ID so a trace survives the gateway hop,
        # and mint one otherwise. Every log line in this request's task reads
        # it from the contextvar; nothing has to thread it through by hand.
        request_id = request.headers.get("x-request-id") or str(uuid.uuid4())
        request_token = app_logging.request_id_var.set(request_id)
        user_token = app_logging.user_id_var.set(None)
        started = time.perf_counter()
        try:
            response = await call_next(request)
            response.headers["X-Request-ID"] = request_id
            if request.url.path not in _QUIET_PATHS:
                logger.info(
                    "request completed",
                    extra={
                        "method": request.method,
                        "path": request.url.path,
                        "status": response.status_code,
                        "duration_ms": round((time.perf_counter() - started) * 1000, 2),
                    },
                )
            return response
        finally:
            app_logging.user_id_var.reset(user_token)
            app_logging.request_id_var.reset(request_token)

    app.include_router(health_router)
    app.include_router(jwks_router)
    app.include_router(auth_router)
    app.include_router(users_router)
    # After users_router, so `/api/users/{user_id}` is already declared and
    # neither route can shadow the other — they differ in path, not in prefix.
    app.include_router(user_ratings_router)
    app.include_router(ratings_router)
    # Never routed through the gateway — see app/api/internal.py and
    # deploy/traefik/dynamic.yml. It is mounted on the service port only.
    app.include_router(internal_router)

    return app
