"""Data access for ratings, rating windows and the per-user aggregate.

Every method takes the caller's transaction and flushes rather than commits:
a submission writes four tables and an outbox row, and either all of them
land or none do (CLAUDE.md rule 4).
"""

from __future__ import annotations

import datetime as dt
import decimal
import uuid
from collections.abc import Sequence
from typing import NamedTuple

from sqlalchemy import Row, and_, func, select, tuple_, update
from sqlalchemy.dialects.postgresql import insert as pg_insert
from sqlalchemy.ext.asyncio import AsyncSession

from app.models import PendingRating, Rating, User, UserRating
from app.models.rating import rating_average
from app.schemas.cursor import Cursor


class Aggregate(NamedTuple):
    """A user's rating totals and the average derived from them.

    `rating_avg` is None until somebody rates them — "not rated yet" and
    "rated zero" are different facts, and only one of them is true of a new
    account.
    """

    rating_sum: int
    rating_count: int
    rating_avg: decimal.Decimal | None

    @staticmethod
    def empty() -> "Aggregate":
        """What a user nobody has rated reads as."""
        return Aggregate(0, 0, None)

    def average(self) -> float | None:
        """The average as the JSON number every response carries.

        Postgres hands back a Decimal; Pydantic renders a Decimal field as the
        string `"4.20"`, and nothing consuming this API wants to parse that
        back into a number. The rounding already happened in the database —
        this only changes the type.
        """
        return None if self.rating_avg is None else float(self.rating_avg)


class PendingTripRow(NamedTuple):
    """One trip on the caller's pending list, before its ratees are attached."""

    trip_id: uuid.UUID
    trip_title: str
    expires_at: dt.datetime


class RatingRepository:
    def __init__(self, session: AsyncSession) -> None:
        self._session = session

    # -- the rating window ---------------------------------------------------

    async def open_windows(
        self,
        *,
        trip_id: uuid.UUID,
        trip_title: str,
        pairs: Sequence[tuple[uuid.UUID, uuid.UUID]],
        expires_at: dt.datetime,
    ) -> int:
        """Insert one pending row per (rater, ratee) pair. Returns rows written.

        `ON CONFLICT DO NOTHING` rather than a pre-check: the consumer is
        idempotent through `processed_events`, but a roster that overlaps a
        previous `trip.completed` for the same trip — a redelivery of an event
        whose id changed, a manual replay from the DLQ — must not raise here
        and dead-letter a message that has nothing wrong with it.

        One statement for every pair, not one per pair: a twelve-person trip
        is 132 rows, and 132 round trips inside a message handler is how a
        consumer falls behind.
        """
        if not pairs:
            return 0

        statement = (
            pg_insert(PendingRating)
            .values(
                [
                    {
                        "id": uuid.uuid4(),
                        "trip_id": trip_id,
                        "trip_title": trip_title,
                        "rater_id": rater_id,
                        "ratee_id": ratee_id,
                        "expires_at": expires_at,
                    }
                    for rater_id, ratee_id in pairs
                ]
            )
            .on_conflict_do_nothing(
                index_elements=[
                    PendingRating.trip_id,
                    PendingRating.rater_id,
                    PendingRating.ratee_id,
                ]
            )
        )
        result = await self._session.execute(statement)
        return result.rowcount or 0

    async def find_open_window(
        self,
        *,
        trip_id: uuid.UUID,
        rater_id: uuid.UUID,
        ratee_id: uuid.UUID,
        now: dt.datetime,
    ) -> PendingRating | None:
        """The one row that authorises a submission, or None.

        `FOR UPDATE` so two submissions racing on the same pair serialise on
        this row. The unique index on `ratings` is what ultimately makes the
        second one fail, but taking the lock here means the loser fails on the
        rule it actually broke rather than on a constraint further down.
        """
        result = await self._session.execute(
            select(PendingRating)
            .where(
                PendingRating.trip_id == trip_id,
                PendingRating.rater_id == rater_id,
                PendingRating.ratee_id == ratee_id,
                PendingRating.completed_at.is_(None),
                PendingRating.expires_at > now,
            )
            .with_for_update()
        )
        return result.scalar_one_or_none()

    async def complete_window(self, pending: PendingRating, *, now: dt.datetime) -> None:
        pending.completed_at = now
        await self._session.flush()

    async def outstanding_trips(
        self,
        *,
        rater_id: uuid.UUID,
        now: dt.datetime,
        limit: int,
        cursor: Cursor | None,
    ) -> list[PendingTripRow]:
        """One page of trips the caller still owes ratings on, soonest first.

        A page is a page of *trips*, so a trip's ratees are never split across
        two of them. Every row of a (trip, rater) group was written by one
        consumer transaction and therefore shares a title and an expiry, which
        is why `min()` over the group is not an arbitrary pick.

        Ordered by expiry rather than by when the trip finished: the list is a
        to-do list, and what a traveller needs from it is what closes first.
        """
        expires_at = func.min(PendingRating.expires_at).label("expires_at")
        statement = (
            select(
                PendingRating.trip_id,
                func.min(PendingRating.trip_title).label("trip_title"),
                expires_at,
            )
            .where(
                PendingRating.rater_id == rater_id,
                PendingRating.completed_at.is_(None),
                # Expiry is applied on read and not left to the nightly sweep.
                # The sweep keeps the table small; it is not what makes the
                # answer correct, and a list that is only right after a cron
                # ran is a list that is wrong every morning.
                PendingRating.expires_at > now,
            )
            .group_by(PendingRating.trip_id)
            .order_by(expires_at, PendingRating.trip_id)
            .limit(limit)
        )
        if cursor is not None:
            statement = statement.having(
                tuple_(func.min(PendingRating.expires_at), PendingRating.trip_id)
                > tuple_(cursor.timestamp, cursor.id)
            )

        result = await self._session.execute(statement)
        return [PendingTripRow(*row) for row in result.all()]

    async def ratees_for_trips(
        self,
        *,
        rater_id: uuid.UUID,
        trip_ids: Sequence[uuid.UUID],
        now: dt.datetime,
    ) -> list[Row]:
        """Who the caller still owes on each of those trips, with display data.

        The join to `users` is inside identity's own database — the display
        name and avatar of a co-traveller are this service's own columns, so
        the pending list needs no call into trip and crosses no service
        boundary. An inner join: a ratee with no user row cannot be rendered,
        and there is nothing useful to show in their place.
        """
        if not trip_ids:
            return []
        result = await self._session.execute(
            select(
                PendingRating.trip_id,
                PendingRating.ratee_id,
                User.full_name,
                User.photo_url,
            )
            .join(User, User.id == PendingRating.ratee_id)
            .where(
                PendingRating.rater_id == rater_id,
                PendingRating.trip_id.in_(trip_ids),
                PendingRating.completed_at.is_(None),
                PendingRating.expires_at > now,
            )
            # Stable and human: alphabetical, with the id as the tiebreaker so
            # two travellers with the same name do not swap places between
            # requests.
            .order_by(User.full_name, PendingRating.ratee_id)
        )
        return list(result.all())

    async def expire_windows(self, *, now: dt.datetime, limit: int) -> int:
        """Close one bounded chunk of windows whose time ran out.

        Bounded by LIMIT and called in a loop, so the nightly sweep never
        takes a long lock or blows out one transaction. `completed_at` is set
        with no rating behind it, which is exactly what "the window closed
        unused" means — the row stays as the record that it was offered.
        """
        doomed = (
            select(PendingRating.id)
            .where(
                PendingRating.completed_at.is_(None),
                PendingRating.expires_at <= now,
            )
            .limit(limit)
            .with_for_update(skip_locked=True)
            .scalar_subquery()
        )
        result = await self._session.execute(
            update(PendingRating)
            .where(PendingRating.id.in_(doomed))
            .values(completed_at=now)
        )
        return result.rowcount or 0

    # -- the ratings themselves ----------------------------------------------

    async def insert_rating(
        self,
        *,
        trip_id: uuid.UUID,
        rater_id: uuid.UUID,
        ratee_id: uuid.UUID,
        score: int,
        comment: str | None,
        now: dt.datetime,
    ) -> Rating:
        rating = Rating(
            id=uuid.uuid4(),
            trip_id=trip_id,
            rater_id=rater_id,
            ratee_id=ratee_id,
            score=score,
            comment=comment,
            created_at=now,
        )
        self._session.add(rating)
        # Flush, not commit: the caller owns the transaction. It is also what
        # surfaces `ratings_one_per_pair` here, where it can be turned into a
        # 409, rather than at commit time where nothing is left to catch it.
        await self._session.flush()
        return rating

    async def rating_exists(
        self, *, trip_id: uuid.UUID, rater_id: uuid.UUID, ratee_id: uuid.UUID
    ) -> bool:
        result = await self._session.execute(
            select(Rating.id).where(
                Rating.trip_id == trip_id,
                Rating.rater_id == rater_id,
                Rating.ratee_id == ratee_id,
            )
        )
        return result.first() is not None

    async def received_ratings(
        self, *, ratee_id: uuid.UUID, limit: int, cursor: Cursor | None
    ) -> list[Row]:
        """One page of ratings a user was given, newest first.

        Two things about this query are the privacy rule rather than a
        preference:

        * `ratings.rater_id` is not selected. The caller cannot receive what
          the query never read.
        * The ordering is fixed and the endpoint takes no filter. A ratee who
          could sort or filter this list — by score, by trip, by date range —
          could intersect it with a roster and work out who said what; with
          one order and no predicates, a page is just a page.

        The title comes from the pending row that authorised the rating: same
        database, same service, and the only place identity ever learned a
        trip's name. Every rating has exactly one such row, because a rating
        cannot exist without it.
        """
        statement = (
            select(
                # The id is read for the keyset only. It is the second half of
                # the sort key, and a page boundary cannot be expressed without
                # it; it is dropped before the response is built.
                Rating.id.label("rating_id"),
                Rating.score,
                Rating.comment,
                Rating.created_at,
                PendingRating.trip_title,
            )
            .join(
                PendingRating,
                and_(
                    PendingRating.trip_id == Rating.trip_id,
                    PendingRating.rater_id == Rating.rater_id,
                    PendingRating.ratee_id == Rating.ratee_id,
                ),
            )
            .where(Rating.ratee_id == ratee_id)
            .order_by(Rating.created_at.desc(), Rating.id.desc())
            .limit(limit)
        )
        if cursor is not None:
            statement = statement.where(
                tuple_(Rating.created_at, Rating.id) < tuple_(cursor.timestamp, cursor.id)
            )
        result = await self._session.execute(statement)
        return list(result.all())

    # -- the aggregate -------------------------------------------------------

    async def increment_aggregate(self, *, user_id: uuid.UUID, score: int) -> Aggregate:
        """Add one score to a user's totals and return the new ones.

        An UPSERT rather than a read-modify-write. Two people rating the same
        traveller at the same moment both read `count = 3`, both write 4, and
        one rating is lost; `rating_sum = user_rating.rating_sum + excluded`
        is computed by the database while it holds the row, so there is no
        such window. The row is created on first rating rather than at
        registration, so the table holds only users somebody has actually
        rated.

        The average comes back from the same statement, computed by Postgres
        from the values it just wrote — the number returned to the client and
        the number put on the bus are one expression evaluated once.
        """
        insert = pg_insert(UserRating).values(
            user_id=user_id, rating_sum=score, rating_count=1
        )
        statement = insert.on_conflict_do_update(
            index_elements=[UserRating.user_id],
            set_={
                "rating_sum": UserRating.rating_sum + insert.excluded.rating_sum,
                "rating_count": UserRating.rating_count + insert.excluded.rating_count,
            },
        ).returning(
            UserRating.rating_sum,
            UserRating.rating_count,
            rating_average(UserRating.rating_sum, UserRating.rating_count).label(
                "rating_avg"
            ),
        )
        row = (await self._session.execute(statement)).one()
        return Aggregate(row.rating_sum, row.rating_count, row.rating_avg)

    async def get_aggregate(self, user_id: uuid.UUID) -> Aggregate:
        """A user's totals, or zeroes if nobody has rated them yet."""
        result = await self._session.execute(
            select(
                UserRating.rating_sum,
                UserRating.rating_count,
                UserRating.rating_avg,
            ).where(UserRating.user_id == user_id)
        )
        row = result.first()
        if row is None:
            return Aggregate.empty()
        return Aggregate(row.rating_sum, row.rating_count, row.rating_avg)
