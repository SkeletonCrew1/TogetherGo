from __future__ import annotations

import datetime as dt
import uuid

from sqlalchemy import Boolean, Date, Text, func
from sqlalchemy.dialects.postgresql import CITEXT, UUID
from sqlalchemy.orm import Mapped, mapped_column

from app.models.base import Base


class User(Base):
    __tablename__ = "users"

    id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True)
    # citext, so `Alex@Example.com` and `alex@example.com` are the same
    # account and the unique index enforces that without a functional index
    # every query would have to remember to match.
    email: Mapped[str] = mapped_column(CITEXT, unique=True, nullable=False)
    password_hash: Mapped[str] = mapped_column(Text, nullable=False)
    full_name: Mapped[str] = mapped_column(Text, nullable=False)
    bio: Mapped[str | None] = mapped_column(Text)
    birth_date: Mapped[dt.date | None] = mapped_column(Date)
    phone: Mapped[str | None] = mapped_column(Text)
    photo_url: Mapped[str | None] = mapped_column(Text)
    is_active: Mapped[bool] = mapped_column(Boolean, nullable=False, server_default="true")
    created_at: Mapped[dt.datetime] = mapped_column(
        server_default=func.now(), nullable=False
    )
    updated_at: Mapped[dt.datetime] = mapped_column(
        server_default=func.now(), onupdate=func.now(), nullable=False
    )

    # There is deliberately no rating column and no relationship here. The
    # aggregate lives in `user_rating`, and every response that shows it is
    # built from a `(User, Aggregate)` pair the repository loads together —
    # see `UserRepository.get_with_rating`. A mapped relationship would be
    # tidier to read and worse to run: it lazy-loads on a row this session
    # just INSERTed, which on an async session is not a slow query but an
    # exception.
