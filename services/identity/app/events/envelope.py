"""The event envelope, duplicated here rather than shared.

CLAUDE.md rule 7: no shared library across languages. This file is the Python
half of a contract whose Go half lives in the trip and chat services, and
`contracts/events.md` is the thing both of them answer to.
"""

from __future__ import annotations

import datetime as dt
import uuid
from typing import Any

USER_REGISTERED = "user.registered"
USER_PROFILE_UPDATED = "user.profile_updated"
USER_RATING_UPDATED = "user.rating_updated"

# Consumed, not published. Identity's only inbound event: it carries the full
# roster of a finished trip, which is what lets this service open the rating
# window without asking trip who travelled.
TRIP_COMPLETED = "trip.completed"

# `event_version` is a property of the publishing build, not of the stored row
# — which is why the outbox does not carry it. Bumping a version here is a
# code change, not a data migration.
EVENT_VERSIONS: dict[str, int] = {
    USER_REGISTERED: 1,
    USER_PROFILE_UPDATED: 1,
    USER_RATING_UPDATED: 1,
}


def rfc3339(value: dt.datetime) -> str:
    """A timestamp as every payload and envelope field on the bus spells it."""
    return value.astimezone(dt.UTC).isoformat().replace("+00:00", "Z")


def build_envelope(
    *,
    event_id: uuid.UUID,
    event_type: str,
    aggregate_id: uuid.UUID,
    occurred_at: dt.datetime,
    payload: dict[str, Any],
) -> dict[str, Any]:
    """Wrap a stored payload in the five envelope fields.

    `occurred_at` is the outbox row's `created_at`: the domain fact happened
    when the transaction wrote it, not when the relay got around to it.
    """
    version = EVENT_VERSIONS.get(event_type)
    if version is None:
        raise KeyError(f"no event_version registered for {event_type!r}")

    return {
        "event_id": str(event_id),
        "event_type": event_type,
        "event_version": version,
        "occurred_at": rfc3339(occurred_at),
        "aggregate_id": str(aggregate_id),
        "payload": payload,
    }
