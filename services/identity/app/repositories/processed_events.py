"""The consumer-side idempotency check (CLAUDE.md rule 5)."""

from __future__ import annotations

import uuid

from sqlalchemy.dialects.postgresql import insert as pg_insert
from sqlalchemy.ext.asyncio import AsyncSession

from app.models import ProcessedEvent


class ProcessedEventRepository:
    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def claim(self, event_id: uuid.UUID) -> bool:
        """Record the event and report whether this delivery was the first.

        Check-then-process in a single statement inside the caller's
        transaction. A `SELECT` followed by an `INSERT` would leave a window
        in which two deliveries of the same event both see nothing and both
        do the work; the primary key closes it, and `ON CONFLICT DO NOTHING`
        turns the loser into `False` instead of an exception the handler
        would have to unpick.

        A `False` means the caller should roll back and acknowledge, having
        done nothing.
        """
        result = await self._session.execute(
            pg_insert(ProcessedEvent)
            .values(event_id=event_id)
            .on_conflict_do_nothing(index_elements=[ProcessedEvent.event_id])
        )
        return (result.rowcount or 0) > 0
