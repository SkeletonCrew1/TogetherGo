"""POST /api/ratings, GET /api/ratings/pending, GET /api/users/{id}/ratings.

Routers hold no business logic: validate, delegate, shape the response. The
one thing this module decides is what a rating looks like on the way out, and
that decision is the privacy boundary — `ReceivedRating` has no field for the
rater, and the endpoint below accepts no `sort` and no filter, so a ratee has
no handle with which to work out who said what about them.
"""

from __future__ import annotations

import uuid
from typing import Annotated

from fastapi import APIRouter, Depends, Query, status

from app.deps import current_user, get_rating_service
from app.models import User
from app.schemas import ErrorResponse
from app.schemas.cursor import MAX_CURSOR_LENGTH, decode_cursor
from app.schemas.rating import (
    DEFAULT_PAGE_SIZE,
    MAX_PAGE_SIZE,
    PendingRatingsResponse,
    RatingSubmitted,
    ReceivedRatingsResponse,
    SubmitRatingRequest,
)
from app.services.rating_service import RatingService

UNAUTHENTICATED = {
    "model": ErrorResponse,
    "description": "`invalid_access_token` — the bearer token is missing or invalid.",
}
BAD_CURSOR = {"model": ErrorResponse, "description": "`invalid_cursor`."}

# Declared once. Both listings paginate the same way, and a `limit` that meant
# something different on two endpoints of one service would be a trap.
LimitParam = Annotated[
    int,
    Query(
        ge=1,
        le=MAX_PAGE_SIZE,
        description=f"Page size, 1–{MAX_PAGE_SIZE}.",
    ),
]
CursorParam = Annotated[
    str | None,
    Query(
        max_length=MAX_CURSOR_LENGTH,
        description="`next_cursor` from the previous page. Opaque; do not parse it.",
    ),
]

router = APIRouter(prefix="/api/ratings", tags=["ratings"])

# A second router because the path belongs to the user, not to the rating:
# a co-traveller's ratings are read from their profile. It is mounted
# alongside `/api/users` and routed to this service by the same gateway rule.
user_ratings_router = APIRouter(prefix="/api/users", tags=["ratings"])


@router.get(
    "/pending",
    response_model=PendingRatingsResponse,
    summary="Trips the caller still owes ratings on",
    response_description=(
        "One entry per trip, soonest expiry first, each listing the "
        "co-travellers the caller has not yet rated. Windows that have "
        "expired or been used are absent."
    ),
    responses={400: BAD_CURSOR, 401: UNAUTHENTICATED},
)
async def list_pending(
    limit: LimitParam = DEFAULT_PAGE_SIZE,
    cursor: CursorParam = None,
    user: User = Depends(current_user),
    service: RatingService = Depends(get_rating_service),
) -> PendingRatingsResponse:
    # Declared before `/{...}`-shaped routes on this prefix would be, so
    # "pending" is never parsed as something else.
    return await service.list_pending(
        user.id, limit=limit, cursor=decode_cursor(cursor)
    )


@router.post(
    "",
    status_code=status.HTTP_201_CREATED,
    response_model=RatingSubmitted,
    summary="Rate a co-traveller",
    response_description=(
        "The stored rating, with the rated user's new aggregate so the caller "
        "can update their card without a second request."
    ),
    responses={
        401: UNAUTHENTICATED,
        403: {
            "model": ErrorResponse,
            "description": (
                "`not_eligible` — there is no open rating window for this "
                "(trip, rater, ratee). One answer for every reason: the trip "
                "has not completed, the fourteen days ran out, or you did not "
                "travel together."
            ),
        },
        409: {
            "model": ErrorResponse,
            "description": (
                "`rating_already_submitted` — a rating is immutable once given."
            ),
        },
    },
)
async def submit_rating(
    body: SubmitRatingRequest,
    user: User = Depends(current_user),
    service: RatingService = Depends(get_rating_service),
) -> RatingSubmitted:
    # The rater is the bearer token and never a body field: a client that
    # could name the rater could rate on someone else's behalf.
    return await service.submit(user.id, body)


@user_ratings_router.get(
    "/{user_id}/ratings",
    response_model=ReceivedRatingsResponse,
    summary="The ratings a traveller has received",
    response_description=(
        "Score, comment, date and trip title, newest first — never who wrote "
        "it. There is no sort or filter parameter, on purpose."
    ),
    responses={
        400: BAD_CURSOR,
        401: UNAUTHENTICATED,
        404: {"model": ErrorResponse, "description": "`user_not_found`."},
    },
)
async def list_received(
    user_id: uuid.UUID,
    limit: LimitParam = DEFAULT_PAGE_SIZE,
    cursor: CursorParam = None,
    _: User = Depends(current_user),
    service: RatingService = Depends(get_rating_service),
) -> ReceivedRatingsResponse:
    # Anyone authenticated may read anyone's ratings, including their own:
    # they are what a traveller looks at before joining a stranger's trip.
    # What nobody may read is who gave them.
    return await service.list_received(
        user_id, limit=limit, cursor=decode_cursor(cursor)
    )
