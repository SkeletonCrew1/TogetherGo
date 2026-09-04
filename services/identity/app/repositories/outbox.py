from __future__ import annotations

import datetime as dt
import uuid
from typing import Any

from sqlalchemy import delete, select, update
from sqlalchemy.ext.asyncio import AsyncSession

from app.models import Outbox


class OutboxRepository:
    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def enqueue(
        self,
        *,
        aggregate_id: uuid.UUID,
        event_type: str,
        payload: dict[str, Any],
    ) -> Outbox:
        """Stage an event inside the caller's transaction.

        The id is generated here rather than by the database because it
        becomes the envelope's `event_id`, and a republish after a relay
        crash has to carry the same one for consumers to deduplicate on.
        """
        row = Outbox(
            id=uuid.uuid4(),
            aggregate_id=aggregate_id,
            event_type=event_type,
            payload=payload,
        )
        self._session.add(row)
        await self._session.flush()
        return row

    async def claim_unpublished(self, limit: int) -> list[Outbox]:
        """Lock a batch for this relay tick.

        `FOR UPDATE SKIP LOCKED` is what makes replicas safe with no leader
        election: a second relay running this query at the same moment skips
        the locked rows and takes the next batch instead of blocking on them
        or publishing them twice.
        """
        result = await self._session.execute(
            select(Outbox)
            .where(Outbox.published_at.is_(None))
            .order_by(Outbox.created_at)
            .limit(limit)
            .with_for_update(skip_locked=True)
        )
        return list(result.scalars())

    async def mark_published(self, ids: list[uuid.UUID], *, now: dt.datetime) -> None:
        if not ids:
            return
        await self._session.execute(
            update(Outbox).where(Outbox.id.in_(ids)).values(published_at=now)
        )

    async def delete_published_before(self, cutoff: dt.datetime, *, limit: int) -> int:
        """Delete one bounded chunk of published rows. Returns the row count.

        Bounded by LIMIT and called repeatedly, so cleanup never takes a long
        lock or blows out a single transaction. Unpublished rows are never
        touched however old — one still NULL after the retention window is
        evidence of a bug, not garbage.
        """
        doomed = (
            select(Outbox.id)
            .where(Outbox.published_at.is_not(None), Outbox.published_at < cutoff)
            .limit(limit)
            .scalar_subquery()
        )
        result = await self._session.execute(delete(Outbox).where(Outbox.id.in_(doomed)))
        return result.rowcount or 0
