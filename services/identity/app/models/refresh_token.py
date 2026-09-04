from __future__ import annotations

import datetime as dt
import uuid

from sqlalchemy import ForeignKey, LargeBinary, Text, func
from sqlalchemy.dialects.postgresql import UUID
from sqlalchemy.orm import Mapped, mapped_column

from app.models.base import Base


class RefreshToken(Base):
    """One row per issued refresh token.

    Rotation makes these a linked list: refreshing revokes the presented row
    and points its `replaced_by` at the successor. A whole list is one login
    session, which is what logout and reuse detection operate on.
    """

    __tablename__ = "refresh_tokens"

    id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True)
    user_id: Mapped[uuid.UUID] = mapped_column(
        UUID(as_uuid=True), ForeignKey("users.id", ondelete="CASCADE"), nullable=False
    )
    # SHA-256 of the token. The token itself is 32 bytes of CSPRNG output, so
    # it needs no password-grade KDF; a digest is enough to make the table
    # useless to anyone who reads it.
    token_hash: Mapped[bytes] = mapped_column(LargeBinary, unique=True, nullable=False)
    issued_at: Mapped[dt.datetime] = mapped_column(server_default=func.now(), nullable=False)
    expires_at: Mapped[dt.datetime] = mapped_column(nullable=False)
    revoked_at: Mapped[dt.datetime | None] = mapped_column()
    replaced_by: Mapped[uuid.UUID | None] = mapped_column(
        UUID(as_uuid=True), ForeignKey("refresh_tokens.id", ondelete="SET NULL")
    )
    user_agent: Mapped[str | None] = mapped_column(Text)
    # The `jti` of the access token minted in the same breath as this refresh
    # token. It is the only handle a bearer token gives us on the session that
    # issued it, and `POST /api/users/me/password` needs exactly that: revoke
    # every session for the account *except* the caller's own. Adding a claim
    # to the token instead would break the claim set CLAUDE.md pins.
    access_token_jti: Mapped[uuid.UUID | None] = mapped_column(UUID(as_uuid=True))
