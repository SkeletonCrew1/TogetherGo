"""GET /healthz and GET /readyz."""

from __future__ import annotations

import logging
from typing import Any

from fastapi import APIRouter, Request, Response, status
from sqlalchemy import text

logger = logging.getLogger(__name__)

router = APIRouter(tags=["health"])


@router.get("/healthz", summary="Liveness")
async def healthz() -> dict[str, str]:
    """Is the process up.

    Checks nothing else on purpose: a liveness probe that fails because
    Postgres is slow gets the container killed and restarted, which cannot
    possibly help.
    """
    return {"status": "ok"}


@router.get("/readyz", summary="Readiness")
async def readyz(request: Request, response: Response) -> dict[str, Any]:
    """Can the process do its job — database reachable, broker reachable."""
    checks: dict[str, str] = {}

    try:
        async with request.app.state.engine.connect() as connection:
            await connection.execute(text("SELECT 1"))
        checks["database"] = "ok"
    except Exception as exc:
        logger.warning("readiness: database check failed", extra={"error_type": type(exc).__name__})
        checks["database"] = "unavailable"

    relay = getattr(request.app.state, "relay", None)
    if relay is None:
        # Relay disabled (tests, or a replica running as API only): there is
        # no broker dependency to report on.
        checks["broker"] = "disabled"
    else:
        checks["broker"] = "ok" if relay.connected else "unavailable"

    # Reported separately from the relay: they are two connections and two
    # directions, and an instance that can publish but cannot consume is
    # opening no rating windows even though the outbox is draining fine.
    consumer = getattr(request.app.state, "consumer", None)
    if consumer is None:
        checks["trip_events"] = "disabled"
    else:
        checks["trip_events"] = "ok" if consumer.connected else "unavailable"

    ready = all(value in {"ok", "disabled"} for value in checks.values())
    if not ready:
        response.status_code = status.HTTP_503_SERVICE_UNAVAILABLE

    return {"status": "ready" if ready else "not_ready", "checks": checks}
