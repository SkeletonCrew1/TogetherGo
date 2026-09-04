"""The consumer-side duplicate absorber (CLAUDE.md rule 5)."""

from __future__ import annotations

import datetime as dt
import uuid

from sqlalchemy import func
from sqlalchemy.dialects.postgresql import UUID
from sqlalchemy.orm import Mapped, mapped_column

from app.models.base import Base


class ProcessedEvent(Base):
    """One row per envelope this service has already applied.

    Keyed on the envelope's `event_id`, which a publisher's outbox keeps
    stable across a republish — that is the whole reason at-least-once
    delivery is safe here. The insert happens in the same transaction as the
    work it guards, so there is no state in which one landed and the other
    did not.
    """

    __tablename__ = "processed_events"

    event_id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True)
    processed_at: Mapped[dt.datetime] = mapped_column(
        server_default=func.now(), nullable=False
    )
