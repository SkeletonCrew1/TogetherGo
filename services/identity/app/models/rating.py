"""The three rating tables: a submitted score, an open window, an aggregate."""

from __future__ import annotations

import datetime as dt
import decimal
import uuid

from sqlalchemy import BigInteger, Integer, Numeric, SmallInteger, Text, cast, func
from sqlalchemy.dialects.postgresql import UUID
from sqlalchemy.orm import Mapped, column_property, mapped_column

from app.models.base import Base

# How long a rating window stays open once a trip completes.
RATING_WINDOW = dt.timedelta(days=14)


def rating_average(sum_column, count_column):
    """`round(sum::numeric / NULLIF(count, 0), 2)` — the displayed average.

    Written once, here, and used both as the mapped `UserRating.rating_avg`
    and in the RETURNING clause of the submission's UPSERT, so the number a
    client reads back and the number `user.rating_updated` carries are
    produced by the same expression rather than by two that agree today.

    NULLIF is what turns "not rated yet" into NULL instead of a division by
    zero, and NULL is the honest answer: an unrated account showing 0.00 out
    of 5 would be a lie about it.
    """
    return func.round(cast(sum_column, Numeric) / func.nullif(count_column, 0), 2)


class Rating(Base):
    """One score one traveller gave another for one trip.

    Immutable by construction: there is no update path anywhere in the
    service, and `ratings_one_per_pair` makes a second submission for the
    same triple a constraint violation rather than an overwrite.
    """

    __tablename__ = "ratings"

    id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True)
    trip_id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), nullable=False)
    # Stored, and never returned to the ratee — see `app/api/ratings.py`. The
    # column exists because the uniqueness rule and the eligibility check are
    # both about *who* rated, not merely how many did.
    rater_id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), nullable=False)
    ratee_id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), nullable=False)
    score: Mapped[int] = mapped_column(SmallInteger, nullable=False)
    comment: Mapped[str | None] = mapped_column(Text)
    created_at: Mapped[dt.datetime] = mapped_column(
        server_default=func.now(), nullable=False
    )


class PendingRating(Base):
    """An open invitation for `rater_id` to rate `ratee_id` for one trip.

    Written by the `trip.completed` consumer, one row per ordered pair of
    distinct roster members, and it is the *only* thing that authorises a
    submission. That is deliberate: eligibility must be answerable from
    identity's own database, so the rating window survives trip being down.
    """

    __tablename__ = "pending_ratings"

    id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True)
    trip_id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), nullable=False)
    trip_title: Mapped[str] = mapped_column(Text, nullable=False)
    rater_id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), nullable=False)
    ratee_id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), nullable=False)
    created_at: Mapped[dt.datetime] = mapped_column(
        server_default=func.now(), nullable=False
    )
    expires_at: Mapped[dt.datetime] = mapped_column(nullable=False)
    # NULL while outstanding. Set by a submission, and by the nightly sweep for
    # a window nobody used — so it reads "no longer actionable", not "rated".
    completed_at: Mapped[dt.datetime | None] = mapped_column()


class UserRating(Base):
    """The running total behind a user's displayed rating.

    Sum and count, never the average. An average cannot be incremented, so
    storing one would force every submission to recompute it from rows it
    would first have to read; a sum is a single `+= score` and the division
    happens on the way out.
    """

    __tablename__ = "user_rating"

    user_id: Mapped[uuid.UUID] = mapped_column(UUID(as_uuid=True), primary_key=True)
    rating_sum: Mapped[int] = mapped_column(BigInteger, nullable=False, server_default="0")
    rating_count: Mapped[int] = mapped_column(Integer, nullable=False, server_default="0")

    # Computed by Postgres on every read and never stored — see
    # `rating_average` above for why the division lives here and not in a
    # column somebody has to remember to keep in step.
    rating_avg: Mapped[decimal.Decimal | None] = column_property(
        rating_average(rating_sum, rating_count)
    )
