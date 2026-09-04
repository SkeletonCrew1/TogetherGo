"""The rating system: the consumer, the submission rules, and the aggregate.

`trip.completed` is fed to `RatingEventHandler` directly rather than through
a broker. That is the same object the consumer calls for every delivery — the
transaction, the `processed_events` claim and the writes are all exercised —
and it keeps the suite free of a RabbitMQ container for behaviour that has
nothing to do with AMQP. The consumer's own concerns (ack, retry, DLQ) are
about the wire, not about ratings.
"""

from __future__ import annotations

import datetime as dt
import uuid

import httpx
import pytest
from fastapi import FastAPI
from sqlalchemy import func, select, text
from sqlalchemy.ext.asyncio import AsyncSession

from app.events.payloads import Envelope
from app.models import Outbox, PendingRating, Rating, UserRating
from app.services.rating_service import RatingEventHandler
from app.services.rating_sweeper import PendingRatingSweeper
from tests.conftest import INTERNAL_TOKEN, Account, login, register_account

# --- helpers ----------------------------------------------------------------


def trip_completed(
    *,
    participants: list[str],
    trip_id: str | None = None,
    title: str = "Carpathians ridge, two days",
    event_id: str | None = None,
) -> dict:
    """A `trip.completed` envelope exactly as contracts/events.md spells it."""
    trip_id = trip_id or str(uuid.uuid4())
    completed_at = dt.datetime.now(dt.UTC)
    return {
        "event_id": event_id or str(uuid.uuid4()),
        "event_type": "trip.completed",
        "event_version": 1,
        "occurred_at": completed_at.isoformat().replace("+00:00", "Z"),
        "aggregate_id": trip_id,
        "payload": {
            "trip_id": trip_id,
            "title": title,
            "organizer_id": participants[0],
            "started_at": (completed_at - dt.timedelta(days=1))
            .isoformat()
            .replace("+00:00", "Z"),
            "completed_at": completed_at.isoformat().replace("+00:00", "Z"),
            "rating_window_closes_at": (completed_at + dt.timedelta(days=14))
            .isoformat()
            .replace("+00:00", "Z"),
            "participants": [
                {
                    "user_id": user_id,
                    "role": "organizer" if index == 0 else "participant",
                    "joined_at": (completed_at - dt.timedelta(days=3))
                    .isoformat()
                    .replace("+00:00", "Z"),
                }
                for index, user_id in enumerate(participants)
            ],
        },
    }


async def deliver(app: FastAPI, envelope: dict) -> bool:
    """Hand one message to the handler the consumer would have called."""
    handler = RatingEventHandler(app.state.sessionmaker)
    return await handler.handle(Envelope(envelope))


async def complete_trip(
    app: FastAPI, accounts: list[Account], **kwargs
) -> tuple[str, dict]:
    """Complete a trip for accounts that already exist, and return its id."""
    envelope = trip_completed(
        participants=[account.id for account in accounts], **kwargs
    )
    await deliver(app, envelope)
    return envelope["payload"]["trip_id"], envelope


async def count(session: AsyncSession, model) -> int:
    return (await session.execute(select(func.count()).select_from(model))).scalar_one()


# --- the consumer -----------------------------------------------------------


async def test_trip_completed_opens_every_ordered_pair(
    app: FastAPI, client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """Four participants owe each of the other three: 4 * 3 = 12 rows."""
    accounts = [await register_account(client) for _ in range(4)]
    trip_id, _ = await complete_trip(app, accounts)

    assert await count(session, PendingRating) == 12

    rows = (
        await session.execute(
            select(PendingRating.rater_id, PendingRating.ratee_id).where(
                PendingRating.trip_id == uuid.UUID(trip_id)
            )
        )
    ).all()
    pairs = {(str(rater), str(ratee)) for rater, ratee in rows}
    ids = [account.id for account in accounts]
    assert pairs == {(a, b) for a in ids for b in ids if a != b}
    # Ordered pairs: rating is not symmetric, and A owing B is a different
    # obligation from B owing A.
    assert all((b, a) in pairs for a, b in pairs)


async def test_trip_completed_is_idempotent(
    app: FastAPI, client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """The same envelope twice writes nothing the second time."""
    accounts = [await register_account(client) for _ in range(4)]
    envelope = trip_completed(participants=[account.id for account in accounts])

    assert await deliver(app, envelope) is True
    # False means "already processed" — the consumer acks it and does nothing.
    assert await deliver(app, envelope) is False

    assert await count(session, PendingRating) == 12
    processed = (
        await session.execute(text("SELECT count(*) FROM processed_events"))
    ).scalar_one()
    assert processed == 1


async def test_a_roster_of_one_produces_nothing(
    app: FastAPI, client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """A trip nobody else came on opens no rating window."""
    alone = await register_account(client)
    await complete_trip(app, [alone])

    assert await count(session, PendingRating) == 0


async def test_a_redelivery_under_a_new_event_id_does_not_duplicate_rows(
    app: FastAPI, client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """`processed_events` is not the only guard; the unique key backs it up.

    A manual replay from the DLQ, or a publisher that regenerated the id,
    gets past the idempotency claim. `ON CONFLICT DO NOTHING` on
    (trip_id, rater_id, ratee_id) is what keeps that from doubling the list.
    """
    accounts = [await register_account(client) for _ in range(3)]
    trip_id = str(uuid.uuid4())
    await deliver(app, trip_completed(participants=[a.id for a in accounts], trip_id=trip_id))
    await deliver(app, trip_completed(participants=[a.id for a in accounts], trip_id=trip_id))

    assert await count(session, PendingRating) == 6


async def test_an_unknown_event_type_is_marked_processed_not_rejected(
    app: FastAPI, session: AsyncSession
) -> None:
    """A message for a newer build is absorbed, not dead-lettered."""
    envelope = {
        "event_id": str(uuid.uuid4()),
        "event_type": "trip.teleported",
        "event_version": 1,
        "occurred_at": "2026-08-31T12:00:00Z",
        "aggregate_id": str(uuid.uuid4()),
        "payload": {},
    }
    assert await deliver(app, envelope) is True
    assert await count(session, PendingRating) == 0


# --- the pending list -------------------------------------------------------


async def test_pending_lists_the_other_travellers_grouped_by_trip(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    accounts = [await register_account(client) for _ in range(3)]
    trip_id, _ = await complete_trip(app, accounts, title="Lviv coffee crawl")

    response = await client.get("/api/ratings/pending", headers=accounts[0].auth)
    assert response.status_code == 200, response.text
    body = response.json()

    assert len(body["items"]) == 1
    item = body["items"][0]
    assert item["trip_id"] == trip_id
    assert item["trip_title"] == "Lviv coffee crawl"
    assert {ratee["user_id"] for ratee in item["ratees"]} == {
        accounts[1].id,
        accounts[2].id,
    }
    assert set(item["ratees"][0]) == {"user_id", "full_name", "photo_url"}
    # Fourteen days from now, give or take the test's own runtime.
    expires_at = dt.datetime.fromisoformat(item["expires_at"])
    assert dt.timedelta(days=13) < expires_at - dt.datetime.now(dt.UTC) <= dt.timedelta(days=14)
    assert body["next_cursor"] is None


async def test_pending_drops_a_trip_once_it_is_rated(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    """The list is what is still owed, so a submission removes one ratee."""
    accounts = [await register_account(client) for _ in range(3)]
    trip_id, _ = await complete_trip(app, accounts)

    await submit(client, accounts[0], trip_id, accounts[1].id, 5)
    body = (await client.get("/api/ratings/pending", headers=accounts[0].auth)).json()
    assert [ratee["user_id"] for ratee in body["items"][0]["ratees"]] == [accounts[2].id]

    await submit(client, accounts[0], trip_id, accounts[2].id, 4)
    body = (await client.get("/api/ratings/pending", headers=accounts[0].auth)).json()
    assert body["items"] == []


async def test_pending_paginates_by_trip(app: FastAPI, client: httpx.AsyncClient) -> None:
    """A page is a page of trips; a trip's ratees are never split across two."""
    accounts = [await register_account(client) for _ in range(3)]
    for index in range(3):
        await complete_trip(app, accounts, title=f"trip {index}")

    first = (
        await client.get(
            "/api/ratings/pending?limit=2", headers=accounts[0].auth
        )
    ).json()
    assert len(first["items"]) == 2
    assert all(len(item["ratees"]) == 2 for item in first["items"])
    assert first["next_cursor"] is not None

    second = (
        await client.get(
            f"/api/ratings/pending?limit=2&cursor={first['next_cursor']}",
            headers=accounts[0].auth,
        )
    ).json()
    assert len(second["items"]) == 1
    assert second["next_cursor"] is None

    seen = [item["trip_id"] for item in first["items"] + second["items"]]
    assert len(set(seen)) == 3


async def test_pending_rejects_a_cursor_it_did_not_issue(
    client: httpx.AsyncClient
) -> None:
    account = await register_account(client)
    response = await client.get(
        "/api/ratings/pending?cursor=not-a-cursor", headers=account.auth
    )
    assert response.status_code == 400
    assert response.json()["error"]["code"] == "invalid_cursor"


async def test_pending_requires_a_token(client: httpx.AsyncClient) -> None:
    response = await client.get("/api/ratings/pending")
    assert response.status_code == 401
    assert response.json()["error"]["code"] == "invalid_access_token"


# --- submission -------------------------------------------------------------


async def submit(
    client: httpx.AsyncClient,
    rater: Account,
    trip_id: str,
    ratee_id: str,
    score: int,
    comment: str | None = None,
) -> httpx.Response:
    body: dict = {"trip_id": trip_id, "ratee_id": ratee_id, "score": score}
    if comment is not None:
        body["comment"] = comment
    return await client.post("/api/ratings", json=body, headers=rater.auth)


async def test_submitting_without_a_pending_row_is_not_eligible(
    client: httpx.AsyncClient
) -> None:
    """No open window, no rating. Identity never asks trip about it."""
    rater = await register_account(client)
    ratee = await register_account(client)

    response = await submit(client, rater, str(uuid.uuid4()), ratee.id, 5)
    assert response.status_code == 403
    assert response.json()["error"]["code"] == "not_eligible"


async def test_submitting_after_the_window_expired_is_not_eligible(
    app: FastAPI, client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """The same 403, and it does not wait for the nightly sweep to be true."""
    accounts = [await register_account(client) for _ in range(2)]
    trip_id, _ = await complete_trip(app, accounts)

    async with session.begin():
        await session.execute(
            text("UPDATE pending_ratings SET expires_at = now() - interval '1 hour'")
        )

    response = await submit(client, accounts[0], trip_id, accounts[1].id, 5)
    assert response.status_code == 403
    assert response.json()["error"]["code"] == "not_eligible"


async def test_rating_yourself_is_not_eligible(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    """No pending row ever pairs someone with themselves, so this is a 403."""
    accounts = [await register_account(client) for _ in range(2)]
    trip_id, _ = await complete_trip(app, accounts)

    response = await submit(client, accounts[0], trip_id, accounts[0].id, 5)
    assert response.status_code == 403
    assert response.json()["error"]["code"] == "not_eligible"


async def test_a_rating_is_created_and_moves_the_aggregate(
    app: FastAPI, client: httpx.AsyncClient, session: AsyncSession
) -> None:
    accounts = [await register_account(client) for _ in range(2)]
    trip_id, _ = await complete_trip(app, accounts)

    response = await submit(client, accounts[0], trip_id, accounts[1].id, 4, "Good company.")
    assert response.status_code == 201, response.text
    body = response.json()
    assert body["score"] == 4
    assert body["comment"] == "Good company."
    assert body["ratee_rating_avg"] == 4.0
    assert body["ratee_rating_count"] == 1

    profile = (
        await client.get(f"/api/users/{accounts[1].id}", headers=accounts[0].auth)
    ).json()
    assert profile["rating_avg"] == 4.0
    assert profile["rating_count"] == 1

    # The window it used is closed, and closed by this submission.
    pending = (
        await session.execute(
            select(PendingRating.completed_at).where(
                PendingRating.trip_id == uuid.UUID(trip_id),
                PendingRating.rater_id == uuid.UUID(accounts[0].id),
            )
        )
    ).scalar_one()
    assert pending is not None


async def test_submitting_twice_is_a_conflict_and_the_aggregate_moves_once(
    app: FastAPI, client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """A rating is immutable. The second POST changes nothing at all."""
    accounts = [await register_account(client) for _ in range(2)]
    trip_id, _ = await complete_trip(app, accounts)

    assert (await submit(client, accounts[0], trip_id, accounts[1].id, 5)).status_code == 201

    second = await submit(client, accounts[0], trip_id, accounts[1].id, 1)
    assert second.status_code == 409
    assert second.json()["error"]["code"] == "rating_already_submitted"

    aggregate = (
        await session.execute(
            select(UserRating.rating_sum, UserRating.rating_count, UserRating.rating_avg)
            .where(UserRating.user_id == uuid.UUID(accounts[1].id))
        )
    ).one()
    assert (aggregate.rating_sum, aggregate.rating_count) == (5, 1)
    assert float(aggregate.rating_avg) == 5.0
    assert await count(session, Rating) == 1


async def test_the_aggregate_of_five_four_four_five_three_reads_four_point_two_zero(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    """21 / 5 = 4.2, rounded to two decimals by the database."""
    ratee = await register_account(client)
    raters = [await register_account(client) for _ in range(5)]

    for rater, score in zip(raters, [5, 4, 4, 5, 3], strict=True):
        trip_id, _ = await complete_trip(app, [ratee, rater])
        response = await submit(client, rater, trip_id, ratee.id, score)
        assert response.status_code == 201, response.text

    profile = (await client.get(f"/api/users/{ratee.id}", headers=raters[0].auth)).json()
    assert profile["rating_count"] == 5
    assert profile["rating_avg"] == pytest.approx(4.20)

    received = (
        await client.get(f"/api/users/{ratee.id}/ratings", headers=raters[0].auth)
    ).json()
    assert received["rating_avg"] == pytest.approx(4.20)
    assert received["rating_count"] == 5


async def test_a_score_outside_one_to_five_is_rejected(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    accounts = [await register_account(client) for _ in range(2)]
    trip_id, _ = await complete_trip(app, accounts)

    for score in (0, 6, -1):
        response = await submit(client, accounts[0], trip_id, accounts[1].id, score)
        assert response.status_code == 422, score
        assert response.json()["error"]["code"] == "validation_error"


async def test_a_blank_comment_is_stored_as_no_comment(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    accounts = [await register_account(client) for _ in range(2)]
    trip_id, _ = await complete_trip(app, accounts)

    response = await submit(client, accounts[0], trip_id, accounts[1].id, 5, "   ")
    assert response.status_code == 201
    assert response.json()["comment"] is None


async def test_a_submission_stages_user_rating_updated(
    app: FastAPI, client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """One outbox row, in the same transaction, never published inline."""
    accounts = [await register_account(client) for _ in range(2)]
    trip_id, _ = await complete_trip(app, accounts)
    await submit(client, accounts[0], trip_id, accounts[1].id, 4)

    row = (
        await session.execute(
            select(Outbox).where(Outbox.event_type == "user.rating_updated")
        )
    ).scalar_one()
    assert str(row.aggregate_id) == accounts[1].id
    assert row.published_at is None
    assert row.payload["user_id"] == accounts[1].id
    assert row.payload["rating_average"] == 4.0
    assert row.payload["rating_count"] == 1
    assert row.payload["updated_at"].endswith("Z")


async def test_a_refused_submission_stages_nothing(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """A 403 leaves no rating, no aggregate and no event behind it."""
    rater = await register_account(client)
    ratee = await register_account(client)
    await submit(client, rater, str(uuid.uuid4()), ratee.id, 5)

    assert await count(session, Rating) == 0
    assert await count(session, UserRating) == 0
    assert await count(session, Outbox) == 2  # the two user.registered rows


# --- reading someone's ratings ----------------------------------------------


async def test_received_ratings_never_name_the_rater(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    """Anonymity is a property of the response model, not of a filter."""
    ratee = await register_account(client)
    rater = await register_account(client)
    trip_id, _ = await complete_trip(app, [ratee, rater], title="Ridge walk")
    await submit(client, rater, trip_id, ratee.id, 5, "Carried the tent without being asked.")

    response = await client.get(f"/api/users/{ratee.id}/ratings", headers=ratee.auth)
    assert response.status_code == 200, response.text
    body = response.json()

    assert len(body["items"]) == 1
    assert set(body["items"][0]) == {"score", "comment", "created_at", "trip_title"}
    assert body["items"][0]["trip_title"] == "Ridge walk"
    # Nothing anywhere in the document points back at who wrote it.
    assert rater.id not in response.text


async def test_received_ratings_take_no_sort_or_filter(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    """Unknown query parameters are ignored, so no ordering can be induced.

    A ratee able to sort by score or filter by trip could intersect the result
    with a roster and work out who said what; the endpoint offers no such
    parameter, and FastAPI does not invent one.
    """
    ratee = await register_account(client)
    raters = [await register_account(client) for _ in range(2)]
    for rater, score in zip(raters, [5, 1], strict=True):
        trip_id, _ = await complete_trip(app, [ratee, rater])
        await submit(client, rater, trip_id, ratee.id, score)

    plain = (await client.get(f"/api/users/{ratee.id}/ratings", headers=ratee.auth)).json()
    steered = (
        await client.get(
            f"/api/users/{ratee.id}/ratings?sort=score&order=asc&rater_id={raters[0].id}",
            headers=ratee.auth,
        )
    ).json()
    assert plain["items"] == steered["items"]


async def test_received_ratings_paginate_newest_first(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    ratee = await register_account(client)
    raters = [await register_account(client) for _ in range(3)]
    for index, rater in enumerate(raters):
        trip_id, _ = await complete_trip(app, [ratee, rater], title=f"trip {index}")
        await submit(client, rater, trip_id, ratee.id, 5)

    first = (
        await client.get(f"/api/users/{ratee.id}/ratings?limit=2", headers=ratee.auth)
    ).json()
    assert [item["trip_title"] for item in first["items"]] == ["trip 2", "trip 1"]
    assert first["next_cursor"] is not None

    second = (
        await client.get(
            f"/api/users/{ratee.id}/ratings?limit=2&cursor={first['next_cursor']}",
            headers=ratee.auth,
        )
    ).json()
    assert [item["trip_title"] for item in second["items"]] == ["trip 0"]
    assert second["next_cursor"] is None


async def test_received_ratings_for_an_unknown_user_is_a_404(
    client: httpx.AsyncClient
) -> None:
    account = await register_account(client)
    response = await client.get(
        f"/api/users/{uuid.uuid4()}/ratings", headers=account.auth
    )
    assert response.status_code == 404
    assert response.json()["error"]["code"] == "user_not_found"


async def test_an_unrated_user_reads_null_not_zero(client: httpx.AsyncClient) -> None:
    """"Not rated yet" and "rated zero" are different facts."""
    account = await register_account(client)
    body = (await client.get(f"/api/users/{account.id}/ratings", headers=account.auth)).json()
    assert body["items"] == []
    assert body["rating_avg"] is None
    assert body["rating_count"] == 0


# --- the nightly sweep ------------------------------------------------------


async def test_the_sweep_closes_windows_past_their_expiry(
    app: FastAPI, client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """Expired rows are marked completed with no rating behind them."""
    accounts = [await register_account(client) for _ in range(2)]
    await complete_trip(app, accounts)
    stale, _ = await complete_trip(app, accounts)

    async with session.begin():
        await session.execute(
            text(
                "UPDATE pending_ratings SET expires_at = now() - interval '1 day' "
                "WHERE trip_id = :trip_id"
            ),
            {"trip_id": stale},
        )

    sweeper = PendingRatingSweeper(sessionmaker=app.state.sessionmaker, interval=3600)
    assert await sweeper.sweep() == 2

    rows = (
        await session.execute(
            select(PendingRating.trip_id, PendingRating.completed_at)
        )
    ).all()
    closed = {str(trip_id) for trip_id, completed_at in rows if completed_at is not None}
    assert closed == {stale}
    # Nothing was rated: the sweep sets completed_at and writes no rating.
    assert await count(session, Rating) == 0


async def test_the_sweep_is_only_housekeeping(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    """Running it twice closes nothing the second time, and changes no answer."""
    accounts = [await register_account(client) for _ in range(2)]
    await complete_trip(app, accounts)

    sweeper = PendingRatingSweeper(sessionmaker=app.state.sessionmaker, interval=3600)
    assert await sweeper.sweep() == 0

    body = (await client.get("/api/ratings/pending", headers=accounts[0].auth)).json()
    assert len(body["items"]) == 1


# --- the aggregate on every user-shaped response ----------------------------


async def test_the_aggregate_reaches_every_profile_shape(
    app: FastAPI, client: httpx.AsyncClient
) -> None:
    """Private, public and internal all read the same `user_rating` row."""
    ratee = await register_account(client)
    rater = await register_account(client)
    trip_id, _ = await complete_trip(app, [ratee, rater])
    await submit(client, rater, trip_id, ratee.id, 5)
    # A fresh token, so the profile read below is not served from a stale one.
    ratee = await login(client, ratee)

    private = (await client.get("/api/users/me", headers=ratee.auth)).json()
    assert (private["rating_avg"], private["rating_count"]) == (5.0, 1)

    public = (await client.get(f"/api/users/{ratee.id}", headers=rater.auth)).json()
    assert (public["rating_avg"], public["rating_count"]) == (5.0, 1)

    internal = (
        await client.get(
            f"/internal/users?ids={ratee.id}",
            headers={"X-Internal-Token": INTERNAL_TOKEN},
        )
    ).json()
    assert (internal["users"][0]["rating_avg"], internal["users"][0]["rating_count"]) == (
        5.0,
        1,
    )
