from __future__ import annotations

import datetime as dt
import uuid

from sqlalchemy import select, text, update
from sqlalchemy.ext.asyncio import AsyncSession

from app.models import RefreshToken


class RefreshTokenRepository:
    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def create(
        self,
        *,
        user_id: uuid.UUID,
        token_hash: bytes,
        expires_at: dt.datetime,
        user_agent: str | None,
        access_token_jti: uuid.UUID | None = None,
    ) -> RefreshToken:
        token = RefreshToken(
            id=uuid.uuid4(),
            user_id=user_id,
            token_hash=token_hash,
            expires_at=expires_at,
            user_agent=user_agent,
            access_token_jti=access_token_jti,
        )
        self._session.add(token)
        await self._session.flush()
        return token

    async def get_by_hash_for_update(self, token_hash: bytes) -> RefreshToken | None:
        """Load the row and hold a row lock for the rest of the transaction.

        This is what makes reuse detection reliable under concurrency. Two
        refreshes racing with the same token serialise here; the first rotates
        and sets `revoked_at`, and the second — which was blocked, so it reads
        the committed state — finds a revoked row and reports a replay.
        Without the lock both could read `revoked_at IS NULL` and both mint a
        successor.
        """
        result = await self._session.execute(
            select(RefreshToken).where(RefreshToken.token_hash == token_hash).with_for_update()
        )
        return result.scalar_one_or_none()

    async def rotate(self, *, old: RefreshToken, new_id: uuid.UUID, now: dt.datetime) -> None:
        old.revoked_at = now
        old.replaced_by = new_id
        await self._session.flush()

    async def revoke_all_for_user(self, user_id: uuid.UUID, *, now: dt.datetime) -> int:
        """Revoke every live token for the user. Returns how many were killed."""
        result = await self._session.execute(
            update(RefreshToken)
            .where(RefreshToken.user_id == user_id, RefreshToken.revoked_at.is_(None))
            .values(revoked_at=now)
        )
        return result.rowcount or 0

    async def revoke_chain(self, token_hash: bytes, *, now: dt.datetime) -> int:
        """Revoke the whole rotation chain the given token belongs to.

        Logout should end one session, not every session the account has, so
        it walks the `replaced_by` linked list rather than the user's tokens.
        The recursion follows both directions — successors (`rt.id =
        c.replaced_by`) and predecessors (`rt.replaced_by = c.id`) — because
        the presented token is usually the newest link but need not be.
        `UNION` deduplicates, which is also what terminates the recursion.
        """
        result = await self._session.execute(
            text(
                """
                WITH RECURSIVE chain AS (
                    SELECT id, replaced_by
                    FROM refresh_tokens
                    WHERE token_hash = :token_hash

                    UNION

                    SELECT rt.id, rt.replaced_by
                    FROM refresh_tokens rt
                    JOIN chain c
                      ON rt.id = c.replaced_by
                      OR rt.replaced_by = c.id
                )
                UPDATE refresh_tokens
                SET revoked_at = :now
                WHERE id IN (SELECT id FROM chain)
                  AND revoked_at IS NULL
                """
            ),
            {"token_hash": token_hash, "now": now},
        )
        return result.rowcount or 0

    async def revoke_all_for_user_except_session(
        self, user_id: uuid.UUID, *, access_token_jti: uuid.UUID, now: dt.datetime
    ) -> int:
        """Revoke every live token for the user except the caller's own session.

        The caller presents an access token, not a refresh token, so the
        session is identified through `access_token_jti` — the link written
        when the pair was minted. What is spared is the whole rotation
        *chain* that row belongs to, not just the row itself: an access token
        stays valid for fifteen minutes, so a client that refreshed a moment
        before changing its password would otherwise ask us to spare a row
        that has already been rotated away, and log itself out.

        The chain walk is the same recursive shape as `revoke_chain`, and for
        the same reason — `replaced_by` is followed in both directions
        because the presented session need not be the newest link.

        An unmatched `access_token_jti` leaves the chain empty and revokes
        everything. That is the safe direction to fail in: a password change
        that signs the user out everywhere is an inconvenience, one that
        leaves a stolen session alive is the thing this endpoint exists to
        prevent.
        """
        result = await self._session.execute(
            text(
                """
                WITH RECURSIVE session_root AS (
                    SELECT id, replaced_by
                    FROM refresh_tokens
                    WHERE user_id = :user_id
                      AND access_token_jti = :access_token_jti
                ),
                chain AS (
                    SELECT id, replaced_by FROM session_root

                    UNION

                    SELECT rt.id, rt.replaced_by
                    FROM refresh_tokens rt
                    JOIN chain c
                      ON rt.id = c.replaced_by
                      OR rt.replaced_by = c.id
                )
                UPDATE refresh_tokens
                SET revoked_at = :now
                WHERE user_id = :user_id
                  AND revoked_at IS NULL
                  AND id NOT IN (SELECT id FROM chain)
                """
            ),
            {"user_id": user_id, "access_token_jti": access_token_jti, "now": now},
        )
        return result.rowcount or 0

    async def live_tokens_for_user(self, user_id: uuid.UUID) -> list[RefreshToken]:
        """Unrevoked, unexpired tokens. Used by tests and diagnostics."""
        now = dt.datetime.now(dt.UTC)
        result = await self._session.execute(
            select(RefreshToken).where(
                RefreshToken.user_id == user_id,
                RefreshToken.revoked_at.is_(None),
                RefreshToken.expires_at > now,
            )
        )
        return list(result.scalars())
