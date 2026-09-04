"""Inbound payloads, parsed strictly.

The other half of `contracts/events.md`, on the reading side. Parsing is
deliberately strict about the fields this service acts on and deliberately
tolerant of everything else: a publisher that adds a field must not break a
consumer that does not know about it, and a publisher that omits `trip_id`
has sent something identity can never act on however many times it is
redelivered.
"""

from __future__ import annotations

import uuid
from typing import Any


class MalformedPayload(ValueError):
    """The body parsed as JSON but is not the event it claims to be.

    Permanent by definition — a redelivery carries the same bytes — so the
    consumer dead-letters it rather than retrying.
    """


class Envelope:
    """The five envelope fields every message on the bus carries."""

    __slots__ = ("event_id", "event_type", "aggregate_id", "occurred_at", "payload")

    def __init__(self, raw: dict[str, Any]) -> None:
        if not isinstance(raw, dict):
            raise MalformedPayload("envelope is not a JSON object")
        self.event_id = _uuid(raw, "event_id")
        self.event_type = _text(raw, "event_type")
        payload = raw.get("payload")
        if not isinstance(payload, dict):
            raise MalformedPayload("envelope carries no payload object")
        self.payload: dict[str, Any] = payload
        self.aggregate_id = raw.get("aggregate_id")
        self.occurred_at = raw.get("occurred_at")


class Participant:
    """One member of a completed trip's roster."""

    __slots__ = ("user_id", "role")

    def __init__(self, user_id: uuid.UUID, role: str) -> None:
        self.user_id = user_id
        self.role = role


class TripCompleted:
    """`trip.completed` — the event that opens a rating window.

    The one deliberately fat payload in the catalogue: it carries the whole
    roster so that identity can decide who may rate whom without calling back
    into trip. That is what keeps the rating window working during a trip
    outage, and it is why this class reads `participants` and never a trip id
    it would then have to resolve.
    """

    __slots__ = ("trip_id", "title", "participants")

    def __init__(self, payload: dict[str, Any]) -> None:
        self.trip_id = _uuid(payload, "trip_id")
        self.title = _text(payload, "title")

        raw = payload.get("participants")
        if not isinstance(raw, list):
            raise MalformedPayload("trip.completed carries no participants array")

        seen: dict[uuid.UUID, Participant] = {}
        for entry in raw:
            if not isinstance(entry, dict):
                raise MalformedPayload("participant entry is not an object")
            user_id = _uuid(entry, "user_id")
            # Deduplicated on the way in. A roster that named someone twice
            # would otherwise produce a self-pair further down, which the
            # `rater_id <> ratee_id` constraint would reject and turn a
            # cosmetic publisher bug into a dead-lettered message.
            seen.setdefault(user_id, Participant(user_id, str(entry.get("role") or "")))
        self.participants = list(seen.values())

    def rating_pairs(self) -> list[tuple[uuid.UUID, uuid.UUID]]:
        """Every ordered pair of distinct participants: who may rate whom.

        Ordered, not unordered — rating is not symmetric, and A rating B is a
        different obligation from B rating A. `n` participants therefore give
        `n * (n - 1)` rows, and a roster of one gives none, which is the right
        answer for a trip nobody else came on.
        """
        ids = [participant.user_id for participant in self.participants]
        return [(rater, ratee) for rater in ids for ratee in ids if rater != ratee]


def _uuid(source: dict[str, Any], field: str) -> uuid.UUID:
    value = source.get(field)
    if isinstance(value, uuid.UUID):
        return value
    if not isinstance(value, str):
        raise MalformedPayload(f"{field} is missing")
    try:
        return uuid.UUID(value)
    except ValueError as exc:
        raise MalformedPayload(f"{field} is not a uuid") from exc


def _text(source: dict[str, Any], field: str) -> str:
    value = source.get(field)
    if not isinstance(value, str) or not value:
        raise MalformedPayload(f"{field} is missing")
    return value
