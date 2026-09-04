"""Everything `/api/ratings` decides, plus the consumer that feeds it.

Ratings live in identity rather than in trip because a rating is a property
of a *user*: `GET /api/users/{id}` has to show the aggregate, and it must be
able to do that without touching trips. That choice is what makes the rules
below implementable at all — eligibility is answered entirely from this
service's own tables, so the rating window keeps working while trip is down.

The rules, all enforced here and none of them client-side:

* A submission needs a matching pending row that has neither expired nor been
  completed. That is the *only* eligibility check; identity never calls trip.
* One score per (trip, rater, ratee), immutable once given.
* Insert, complete the window, increment the aggregate and stage
  `user.rating_updated` in one transaction. The aggregate is maintained by
  that increment and is never recomputed by a batch job.
* Ratings are anonymous to the ratee.
"""

from __future__ import annotations

import datetime as dt
import logging
import uuid
from collections import defaultdict

from sqlalchemy.exc import IntegrityError
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from app.errors import NotEligibleToRate, RatingAlreadySubmitted, UserNotFound
from app.events.envelope import TRIP_COMPLETED, USER_RATING_UPDATED, rfc3339
from app.events.payloads import Envelope, TripCompleted
from app.models import RATING_WINDOW
from app.repositories import (
    OutboxRepository,
    ProcessedEventRepository,
    RatingRepository,
    UserRepository,
)
from app.repositories.ratings import Aggregate
from app.schemas.cursor import Cursor
from app.schemas.rating import (
    PendingRatee,
    PendingRatingsResponse,
    PendingTrip,
    RatingSubmitted,
    ReceivedRating,
    ReceivedRatingsResponse,
    SubmitRatingRequest,
)

logger = logging.getLogger(__name__)


class RatingService:
    def __init__(self, session: AsyncSession) -> None:
        self._session = session
        self._ratings = RatingRepository(session)
        self._users = UserRepository(session)
        self._outbox = OutboxRepository(session)
        self._processed = ProcessedEventRepository(session)

    # -- reads --------------------------------------------------------------

    async def list_pending(
        self, rater_id: uuid.UUID, *, limit: int, cursor: Cursor | None
    ) -> PendingRatingsResponse:
        """The caller's outstanding rating windows, grouped by trip.

        Grouped because that is the question being asked — "who do I still owe
        a rating for this trip" — and because the client renders one card per
        trip. Paginating by trip rather than by row is what keeps a trip's
        ratees from being split across two pages.
        """
        now = dt.datetime.now(dt.UTC)
        trips = await self._ratings.outstanding_trips(
            rater_id=rater_id, now=now, limit=limit + 1, cursor=cursor
        )

        # One row over the page size is the "is there more" probe; it is
        # dropped before anything is rendered, so a client never sees it.
        has_more = len(trips) > limit
        trips = trips[:limit]

        rows = await self._ratings.ratees_for_trips(
            rater_id=rater_id, trip_ids=[trip.trip_id for trip in trips], now=now
        )
        by_trip: dict[uuid.UUID, list[PendingRatee]] = defaultdict(list)
        for row in rows:
            by_trip[row.trip_id].append(
                PendingRatee(
                    user_id=row.ratee_id,
                    full_name=row.full_name,
                    photo_url=row.photo_url,
                )
            )

        items = [
            PendingTrip(
                trip_id=trip.trip_id,
                trip_title=trip.trip_title,
                expires_at=trip.expires_at,
                ratees=by_trip[trip.trip_id],
            )
            for trip in trips
            # A trip whose every ratee has vanished from `users` has nothing
            # to render, so it is not a card with an empty list — it is not a
            # card. In practice this never fires: identity does not delete
            # user rows.
            if by_trip[trip.trip_id]
        ]

        next_cursor = (
            Cursor(timestamp=trips[-1].expires_at, id=trips[-1].trip_id).encode()
            if has_more and trips
            else None
        )
        return PendingRatingsResponse(items=items, next_cursor=next_cursor)

    async def list_received(
        self, ratee_id: uuid.UUID, *, limit: int, cursor: Cursor | None
    ) -> ReceivedRatingsResponse:
        """One page of the ratings a user has been given.

        Anonymous: the query never reads `rater_id`, so no serialiser has to
        remember to drop it. The order is fixed and there are no filters — a
        ratee able to sort or filter this list could intersect it with a
        roster and work out who said what.

        The 404 is checked first so that asking about a user who does not
        exist is not answerable as "exists, no ratings".
        """
        user = await self._users.get_by_id(ratee_id)
        if user is None:
            raise UserNotFound()

        rows = await self._ratings.received_ratings(
            ratee_id=ratee_id, limit=limit + 1, cursor=cursor
        )
        has_more = len(rows) > limit
        rows = rows[:limit]

        aggregate = await self._ratings.get_aggregate(ratee_id)
        next_cursor = None
        if has_more and rows:
            # (created_at, id) — the id is read for the ordering and encoded
            # into the opaque cursor, and never rendered on its own: a stable
            # per-rating handle is one more thing a ratee could correlate
            # across pages.
            next_cursor = Cursor(
                timestamp=rows[-1].created_at, id=rows[-1].rating_id
            ).encode()

        return ReceivedRatingsResponse(
            items=[
                ReceivedRating(
                    score=row.score,
                    comment=row.comment,
                    created_at=row.created_at,
                    trip_title=row.trip_title,
                )
                for row in rows
            ],
            next_cursor=next_cursor,
            rating_avg=aggregate.average(),
            rating_count=aggregate.rating_count,
        )

    # -- submission ---------------------------------------------------------

    async def submit(self, rater_id: uuid.UUID, data: SubmitRatingRequest) -> RatingSubmitted:
        """Record one rating, in one transaction.

        Order matters. The 409 is checked before the 403 so that a client
        re-sending a submission it already made is told what actually
        happened: the pending row has been completed by that first submission,
        so an eligibility-first check would answer "not eligible" and send the
        user looking for a window that closed because they used it.
        """
        now = dt.datetime.now(dt.UTC)

        if await self._ratings.rating_exists(
            trip_id=data.trip_id, rater_id=rater_id, ratee_id=data.ratee_id
        ):
            raise RatingAlreadySubmitted()

        pending = await self._ratings.find_open_window(
            trip_id=data.trip_id,
            rater_id=rater_id,
            ratee_id=data.ratee_id,
            now=now,
        )
        if pending is None:
            # Covers every "no": trip unknown to identity, window closed,
            # never travelled together, rating yourself. One answer for all of
            # them, so the endpoint is not an oracle for who was on a trip.
            logger.info(
                "rating refused",
                extra={
                    "user_id": str(rater_id),
                    "trip_id": str(data.trip_id),
                    "reason": "no_open_window",
                },
            )
            raise NotEligibleToRate()

        try:
            rating = await self._ratings.insert_rating(
                trip_id=data.trip_id,
                rater_id=rater_id,
                ratee_id=data.ratee_id,
                score=data.score,
                comment=data.comment,
                now=now,
            )
        except IntegrityError as exc:
            # `ratings_one_per_pair` under a race the SELECT above cannot see.
            # The row lock on the pending row makes this all but unreachable;
            # it is here because "all but" is not "never", and the honest
            # answer to the loser is the same 409 the sequential path gives.
            await self._session.rollback()
            raise RatingAlreadySubmitted() from exc

        await self._ratings.complete_window(pending, now=now)
        aggregate = await self._ratings.increment_aggregate(
            user_id=data.ratee_id, score=data.score
        )
        await self._publish_rating_updated(data.ratee_id, aggregate, now=now)
        await self._session.commit()

        logger.info(
            "rating submitted",
            extra={
                "user_id": str(rater_id),
                "trip_id": str(data.trip_id),
                # The score and the comment are the rater's private opinion of
                # someone; the log records that a rating happened, not what it
                # said.
                "ratee_rating_count": aggregate.rating_count,
            },
        )

        return RatingSubmitted(
            id=rating.id,
            trip_id=rating.trip_id,
            ratee_id=rating.ratee_id,
            score=rating.score,
            comment=rating.comment,
            created_at=rating.created_at,
            ratee_rating_avg=aggregate.average(),
            ratee_rating_count=aggregate.rating_count,
        )

    async def _publish_rating_updated(
        self, ratee_id: uuid.UUID, aggregate: Aggregate, *, now: dt.datetime
    ) -> None:
        """Stage `user.rating_updated` inside the caller's transaction.

        Never published inline (CLAUDE.md rule 4): the row and the outbox row
        commit together, so there is no window in which someone's rating has
        changed and the event announcing it has not, nor one in which an event
        went out for a rating that was rolled back.

        `rating_average` goes out as a JSON number rather than the Decimal
        Postgres returned — JSONB would normalise it anyway, and a consumer
        parsing `"4.20"` as a string is a bug waiting for its first reader.
        """
        await self._outbox.enqueue(
            aggregate_id=ratee_id,
            event_type=USER_RATING_UPDATED,
            payload={
                "user_id": str(ratee_id),
                "rating_average": aggregate.average(),
                "rating_count": aggregate.rating_count,
                "updated_at": rfc3339(now),
            },
        )

    # -- the nightly sweep ---------------------------------------------------

    async def expire_windows(self, *, limit: int) -> int:
        """Close one chunk of windows whose fourteen days ran out.

        Housekeeping, not correctness: every read already filters on
        `expires_at`, so a sweep that never ran would change no answer. What
        it buys is a pending table and a partial index that stay the size of
        the work actually outstanding.
        """
        now = dt.datetime.now(dt.UTC)
        async with self._session.begin():
            return await self._ratings.expire_windows(now=now, limit=limit)

    # -- the consumer's half -------------------------------------------------

    async def handle(self, envelope: Envelope) -> bool:
        """Apply one message from `identity.trip-events`.

        Returns whether this delivery was the first. The idempotency claim and
        the writes are one transaction (CLAUDE.md rule 5), so a duplicate
        rolls back to exactly nothing and a crash mid-handler leaves neither
        the claim nor the rows.
        """
        async with self._session.begin():
            if not await self._processed.claim(envelope.event_id):
                return False

            if envelope.event_type != TRIP_COMPLETED:
                # Marked processed and acknowledged, not dead-lettered: this is
                # a message for a newer version of this service, not a failure.
                logger.warning(
                    "unknown event type ignored",
                    extra={
                        "event_id": str(envelope.event_id),
                        "event_type": envelope.event_type,
                    },
                )
                return True

            await self._open_rating_window(envelope)
            return True

    async def _open_rating_window(self, envelope: Envelope) -> None:
        """Turn a completed trip's roster into pending rating rows.

        One row per *ordered* pair of distinct participants: rating is not
        symmetric, so four travellers give twelve rows and a trip somebody
        took alone gives none.
        """
        payload = TripCompleted(envelope.payload)
        pairs = payload.rating_pairs()

        # `now() + 14 days` rather than the payload's `rating_window_closes_at`
        # — the two agree to within however long this message waited on the
        # queue, and taking our own clock means a window is never already shut
        # when it opens because a consumer was down for an afternoon.
        expires_at = dt.datetime.now(dt.UTC) + RATING_WINDOW

        written = await self._ratings.open_windows(
            trip_id=payload.trip_id,
            trip_title=payload.title,
            pairs=pairs,
            expires_at=expires_at,
        )
        logger.info(
            "rating window opened",
            extra={
                "event_id": str(envelope.event_id),
                "trip_id": str(payload.trip_id),
                "participants": len(payload.participants),
                "pending_created": written,
            },
        )


class RatingEventHandler:
    """Adapter that gives the consumer a fresh session per message.

    The consumer knows nothing about SQLAlchemy and the service knows nothing
    about AMQP; this is the seam. A session per message rather than one for
    the consumer's lifetime, because a failed transaction must not leave the
    next message with a poisoned session.
    """

    def __init__(self, sessionmaker: async_sessionmaker[AsyncSession]) -> None:
        self._sessionmaker = sessionmaker

    async def handle(self, envelope: Envelope) -> bool:
        async with self._sessionmaker() as session:
            return await RatingService(session).handle(envelope)
