"""The wire shapes of `/api/ratings` and `/api/users/{id}/ratings`.

The privacy boundary of this feature is a property of the models here, not of
a filter someone has to remember to apply: `ReceivedRating` has no field for
the rater, so no code path can fill one in even by accident.
"""

from __future__ import annotations

import datetime as dt
import uuid
from typing import TYPE_CHECKING, Self

from pydantic import BaseModel, Field, field_validator

if TYPE_CHECKING:  # pragma: no cover - typing only
    from app.models import Rating

# The rating scale, in one place. The database repeats it as a check
# constraint, because a bound that lives only in the application is a bound
# the next writer does not have.
MIN_SCORE = 1
MAX_SCORE = 5

COMMENT_MAX_LENGTH = 1000

# Page sizes. Both listings are naturally small — a traveller has a handful of
# open windows, and a well-travelled one a few hundred ratings — so the
# default is generous and the ceiling is about bounding a hostile request.
DEFAULT_PAGE_SIZE = 20
MAX_PAGE_SIZE = 100


# --- requests ---------------------------------------------------------------


class SubmitRatingRequest(BaseModel):
    """POST /api/ratings."""

    trip_id: uuid.UUID
    ratee_id: uuid.UUID
    score: int = Field(ge=MIN_SCORE, le=MAX_SCORE)
    # Optional, and an empty or whitespace-only string is stored as no comment
    # rather than as "". A rating with a blank comment renders a blank quote.
    comment: str | None = Field(default=None, max_length=COMMENT_MAX_LENGTH)

    @field_validator("comment")
    @classmethod
    def _blank_is_no_comment(cls, value: str | None) -> str | None:
        if value is None:
            return None
        stripped = value.strip()
        return stripped or None


# --- responses --------------------------------------------------------------


class PendingRatee(BaseModel):
    """One person the caller still owes a rating, on one trip.

    The display fields come from identity's own `users` table — this service
    owns them, so the pending list needs no call into trip and no join across
    a service boundary.
    """

    user_id: uuid.UUID
    full_name: str
    photo_url: str | None = None


class PendingTrip(BaseModel):
    """The caller's outstanding ratings for one completed trip."""

    trip_id: uuid.UUID
    trip_title: str
    expires_at: dt.datetime
    ratees: list[PendingRatee]


class PendingRatingsResponse(BaseModel):
    """`GET /api/ratings/pending`, grouped by trip and keyset paginated.

    Grouped because that is how it is asked: "who do I still owe a rating
    for this trip", not "list 6 rows". A page is a page of *trips*, so a
    trip's ratees are never split across two of them.
    """

    items: list[PendingTrip]
    next_cursor: str | None = None


class ReceivedRating(BaseModel):
    """One rating a user was given, as that user (or anyone) may see it.

    There is no `rater_id`, no `rater` object and no trip_id — only the score,
    the words, when it happened, and which trip it was for. Ratings are
    anonymous to the ratee, and a model with nowhere to put an identity is a
    stronger guarantee of that than a `del` in a serializer.
    """

    score: int
    comment: str | None = None
    created_at: dt.datetime
    trip_title: str

    @classmethod
    def from_row(cls, rating: Rating, trip_title: str) -> Self:
        return cls(
            score=rating.score,
            comment=rating.comment,
            created_at=rating.created_at,
            trip_title=trip_title,
        )


class ReceivedRatingsResponse(BaseModel):
    """`GET /api/users/{id}/ratings`.

    The summary is carried alongside the page so a profile view needs one
    request rather than two, and so a caller reading page three still knows
    what it is a page of.
    """

    items: list[ReceivedRating]
    next_cursor: str | None = None
    rating_avg: float | None = None
    rating_count: int


class RatingSubmitted(BaseModel):
    """The 201 body of `POST /api/ratings`.

    Echoes the aggregate the submission produced, so the client can update
    the rated user's card without a follow-up read.
    """

    id: uuid.UUID
    trip_id: uuid.UUID
    ratee_id: uuid.UUID
    score: int
    comment: str | None = None
    created_at: dt.datetime
    ratee_rating_avg: float | None = None
    ratee_rating_count: int
