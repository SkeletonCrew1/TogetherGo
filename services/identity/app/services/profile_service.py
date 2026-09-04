"""Reading and changing a user's profile, and resolving users in bulk.

All the business rules for `/api/users` and `/internal/users` live here.
Routers validate, delegate and shape a response; they make no decisions.
"""

from __future__ import annotations

import asyncio
import datetime as dt
import logging
import uuid
from collections.abc import Sequence

from sqlalchemy.ext.asyncio import AsyncSession

from app.errors import InvalidCurrentPassword, UserNotFound, WeakPassword
from app.events.envelope import USER_PROFILE_UPDATED, rfc3339
from app.models import User
from app.repositories import (
    Aggregate,
    OutboxRepository,
    RatingRepository,
    RefreshTokenRepository,
    UserRepository,
)
from app.schemas.password_policy import contains_email_local_part
from app.schemas.user import ChangePasswordRequest, UpdateProfileRequest
from app.services import passwords

logger = logging.getLogger(__name__)

# The fields the trip service keeps a local copy of. A change to one of them
# has to reach that projection, so it is what makes a PATCH publish
# `user.profile_updated`. `bio` and `phone` are not projected anywhere — a
# user editing their bio should not put a message on the bus that every
# consumer then has to decide to ignore.
PROJECTED_FIELDS = ("full_name", "photo_url")


class ProfileService:
    def __init__(self, session: AsyncSession) -> None:
        self._session = session
        self._users = UserRepository(session)
        self._refresh_tokens = RefreshTokenRepository(session)
        self._outbox = OutboxRepository(session)

    # -- reads --------------------------------------------------------------

    async def get_profile(self, user_id: uuid.UUID) -> User:
        """One user, or a 404.

        Deactivated accounts are returned like any other. `is_active` governs
        whether someone can sign in; the account still exists, and a trip
        whose roster includes them should still be able to show who they are.
        """
        user = await self._users.get_by_id(user_id)
        if user is None:
            raise UserNotFound()
        return user

    async def get_profile_view(self, user_id: uuid.UUID) -> tuple[User, Aggregate]:
        """One user and the rating aggregate their profile shows, or a 404.

        Every user-shaped response carries the aggregate, so it is loaded with
        the row rather than looked up after it — one query, one round trip.
        """
        found = await self._users.get_with_rating(user_id)
        if found is None:
            raise UserNotFound()
        return found

    async def rating_of(self, user_id: uuid.UUID) -> Aggregate:
        """The aggregate for a user already in hand — a primary-key lookup.

        Used where the row was loaded for some other reason (the caller's own
        `/me`, an account that has just signed in), so re-reading the user to
        get the join would be the wasteful half of the trade.
        """
        return await RatingRepository(self._session).get_aggregate(user_id)

    async def resolve_users(
        self, user_ids: Sequence[uuid.UUID]
    ) -> Sequence[tuple[User, Aggregate]]:
        """Batch resolution for other services. Unknown ids are simply absent."""
        return await self._users.get_many_with_rating(user_ids)

    # -- profile edits ------------------------------------------------------

    async def update_profile(self, user: User, data: UpdateProfileRequest) -> User:
        """Apply a PATCH and, if it touched a projected field, publish.

        The row and the outbox row are one transaction, so there is no window
        in which the profile has changed and the event announcing it has not.
        """
        changes = data.changes()
        if not changes:
            # An empty PATCH is not an error, it is a no-op. Committing
            # nothing and publishing nothing is the honest response to it.
            return user

        # Compared before anything is applied, and against the stored value
        # rather than merely against "was this key present": a PATCH that
        # sets full_name to the name it already had has changed nothing, and
        # publishing for it would wake every consumer for no reason.
        projected_changed = any(
            field in changes and changes[field] != getattr(user, field)
            for field in PROJECTED_FIELDS
        )

        now = dt.datetime.now(dt.UTC)
        await self._users.update_profile(user, changes, now=now)

        if projected_changed:
            await self._outbox.enqueue(
                aggregate_id=user.id,
                event_type=USER_PROFILE_UPDATED,
                payload={
                    "user_id": str(user.id),
                    "display_name": user.full_name,
                    "avatar_url": user.photo_url,
                    # Carried even though a bio change alone does not publish:
                    # contracts/events.md specifies the payload as the full
                    # current profile, not a delta, so a consumer can apply it
                    # without knowing which field moved.
                    "bio": user.bio,
                    "updated_at": rfc3339(now),
                },
            )

        await self._session.commit()
        logger.info(
            "profile updated",
            extra={
                "user_id": str(user.id),
                # Field names only. The values are the user's own bio and
                # phone number and have no business in a log line.
                "fields": ",".join(changes),
                "published": projected_changed,
            },
        )
        return user

    # -- password -----------------------------------------------------------

    async def change_password(
        self,
        user: User,
        data: ChangePasswordRequest,
        *,
        access_token_jti: uuid.UUID,
    ) -> int:
        """Re-authenticate, rehash, and end every other session.

        Returns the number of sessions revoked. Changing a password is what
        someone does after suspecting their account is compromised, so it has
        to invalidate refresh tokens the attacker may hold — and it must not
        invalidate the caller's own, because logging you out of the tab you
        just used is not a security property, it is an annoyance.
        """
        valid = await asyncio.to_thread(
            passwords.verify_password, user.password_hash, data.current_password
        )
        if not valid:
            logger.info("password change refused", extra={"user_id": str(user.id)})
            raise InvalidCurrentPassword()

        # The one strength rule the request body cannot check for itself: it
        # needs the account's email address, which this body does not carry.
        if contains_email_local_part(data.new_password, user.email):
            raise WeakPassword(
                "The new password must not contain the local part of the email address."
            )

        new_hash = await asyncio.to_thread(passwords.hash_password, data.new_password)
        now = dt.datetime.now(dt.UTC)

        await self._users.update_password_hash(user, new_hash)
        user.updated_at = now
        revoked = await self._refresh_tokens.revoke_all_for_user_except_session(
            user.id, access_token_jti=access_token_jti, now=now
        )
        await self._session.commit()

        logger.info(
            "password changed",
            extra={"user_id": str(user.id), "sessions_revoked": revoked},
        )
        return revoked
