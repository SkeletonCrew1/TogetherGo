from __future__ import annotations

import datetime as dt
import uuid
from collections.abc import Iterable, Sequence
from datetime import date

from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from app.models import User, UserRating
from app.repositories.ratings import Aggregate


class UserRepository:
    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    async def get_by_email(self, email: str) -> User | None:
        # The column is citext, so the comparison is case-insensitive in the
        # database and uses the unique index.
        result = await self._session.execute(select(User).where(User.email == email))
        return result.scalar_one_or_none()

    async def get_by_id(self, user_id: uuid.UUID) -> User | None:
        return await self._session.get(User, user_id)

    async def get_with_rating(self, user_id: uuid.UUID) -> tuple[User, Aggregate] | None:
        """One user and the rating aggregate every profile shows, in one query.

        The aggregate is a LEFT JOIN and not a second lookup: a user with no
        `user_rating` row has simply not been rated, which is a shape of the
        answer rather than a missing one.
        """
        result = await self._session.execute(
            select(
                User,
                UserRating.rating_sum,
                UserRating.rating_count,
                UserRating.rating_avg,
            )
            .outerjoin(UserRating, UserRating.user_id == User.id)
            .where(User.id == user_id)
        )
        row = result.first()
        return None if row is None else (row[0], _aggregate(row))

    async def get_many_with_rating(
        self, user_ids: Iterable[uuid.UUID]
    ) -> Sequence[tuple[User, Aggregate]]:
        """The batch resolver's read: many users and their aggregates, one query.

        The join is what keeps `/internal/users` a single round trip. Resolving
        the aggregate per user would be the N+1 the endpoint exists to prevent,
        just moved one layer down.
        """
        ids = list(user_ids)
        if not ids:
            return []
        result = await self._session.execute(
            select(
                User,
                UserRating.rating_sum,
                UserRating.rating_count,
                UserRating.rating_avg,
            )
            .outerjoin(UserRating, UserRating.user_id == User.id)
            .where(User.id.in_(ids))
        )
        return [(row[0], _aggregate(row)) for row in result.all()]

    async def get_many_by_id(self, user_ids: Iterable[uuid.UUID]) -> Sequence[User]:
        """Every user among the given ids that exists. One query, one round trip.

        Ids that match nothing are simply not in the result — the internal
        resolver's contract is that callers render a placeholder for a
        deleted user rather than that the whole batch fails because one
        member of a trip closed their account.

        Deactivated accounts *are* returned. `is_active` is about whether
        someone can sign in; the row still exists and a trip they are on
        should still show their name.
        """
        ids = list(user_ids)
        if not ids:
            return []
        result = await self._session.execute(select(User).where(User.id.in_(ids)))
        return list(result.scalars())

    async def create(
        self,
        *,
        user_id: uuid.UUID,
        email: str,
        password_hash: str,
        full_name: str,
        birth_date: date | None,
        bio: str | None,
    ) -> User:
        user = User(
            id=user_id,
            email=email,
            password_hash=password_hash,
            full_name=full_name,
            birth_date=birth_date,
            bio=bio,
        )
        self._session.add(user)
        # Flush rather than commit: the caller owns the transaction, and the
        # user row and its outbox row must land together or not at all. The
        # flush is what surfaces a duplicate-email violation here instead of
        # at commit time.
        await self._session.flush()
        return user

    async def update_password_hash(self, user: User, password_hash: str) -> None:
        user.password_hash = password_hash
        await self._session.flush()

    async def update_profile(
        self, user: User, changes: dict[str, str | None], *, now: dt.datetime
    ) -> User:
        """Apply the fields a PATCH actually sent.

        `updated_at` is set explicitly rather than left to the column's
        `onupdate`, because the same value goes into the `user.profile_updated`
        payload and consumers use it to discard out-of-order updates. A row
        and an event that disagree about when the change happened would make
        that comparison a coin flip.
        """
        for field, value in changes.items():
            setattr(user, field, value)
        user.updated_at = now
        await self._session.flush()
        return user


def _aggregate(row) -> Aggregate:
    """Read the three joined columns, or zeroes where the join found nothing."""
    if row.rating_count is None:
        return Aggregate.empty()
    return Aggregate(row.rating_sum, row.rating_count, row.rating_avg)
