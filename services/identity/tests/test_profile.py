"""Profile reads and edits, and the line between private and public."""

from __future__ import annotations

import datetime as dt
import uuid

import httpx
import pytest
from sqlalchemy import select, text
from sqlalchemy.ext.asyncio import AsyncSession

from app.models import Outbox
from tests.conftest import register_account


async def test_me_returns_the_full_own_profile(client: httpx.AsyncClient) -> None:
    account = await register_account(client, birth_date="1994-05-17")

    response = await client.get("/api/users/me", headers=account.auth)

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["id"] == account.id
    assert body["email"] == account.email
    # The account holder sees their own contact details and rating.
    assert set(body) >= {"email", "phone", "birth_date", "age", "rating_avg", "rating_count"}
    assert body["rating_avg"] is None
    assert body["rating_count"] == 0


async def test_me_requires_a_bearer_token(client: httpx.AsyncClient) -> None:
    anonymous = await client.get("/api/users/me")

    assert anonymous.status_code == 401
    assert anonymous.json()["error"]["code"] == "invalid_access_token"
    assert anonymous.headers["www-authenticate"] == "Bearer"

    garbage = await client.get(
        "/api/users/me", headers={"Authorization": "Bearer not-a-token"}
    )
    assert garbage.status_code == 401
    # A malformed token and a missing one are the same answer: which of the
    # two it was is not information a caller needs.
    assert garbage.json() == anonymous.json()


async def test_a_refresh_token_cannot_be_used_as_a_bearer_token(
    client: httpx.AsyncClient,
) -> None:
    account = await register_account(client)

    response = await client.get(
        "/api/users/me", headers={"Authorization": f"Bearer {account.refresh_token}"}
    )

    assert response.status_code == 401


async def test_age_is_computed_and_birth_date_is_not_public(
    client: httpx.AsyncClient,
) -> None:
    today = dt.datetime.now(dt.UTC).date()
    # A birthday that has already happened this year, so the arithmetic is
    # unambiguous whenever the suite runs.
    birth_date = today.replace(year=today.year - 30) - dt.timedelta(days=1)
    subject = await register_account(client, birth_date=birth_date.isoformat())
    viewer = await register_account(client)

    response = await client.get(f"/api/users/{subject.id}", headers=viewer.auth)

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["age"] == 30
    assert "birth_date" not in body


async def test_public_profile_carries_no_email_and_no_phone(
    client: httpx.AsyncClient,
) -> None:
    subject = await register_account(client)
    patched = await client.patch(
        "/api/users/me", json={"phone": "+441234567890"}, headers=subject.auth
    )
    assert patched.status_code == 200, patched.text

    viewer = await register_account(client)
    response = await client.get(f"/api/users/{subject.id}", headers=viewer.auth)

    assert response.status_code == 200, response.text
    body = response.json()
    assert set(body) == {
        "id",
        "full_name",
        "bio",
        "photo_url",
        "age",
        "rating_avg",
        "rating_count",
        "created_at",
    }
    # Belt and braces: the exact-set assertion above already proves this, but
    # this is the assertion that names what would be a privacy incident.
    assert "email" not in body
    assert "phone" not in body
    assert subject.email not in response.text
    assert "+441234567890" not in response.text


async def test_public_profile_of_an_unknown_user_is_404(client: httpx.AsyncClient) -> None:
    viewer = await register_account(client)

    response = await client.get(f"/api/users/{uuid.uuid4()}", headers=viewer.auth)

    assert response.status_code == 404
    assert response.json()["error"]["code"] == "user_not_found"


async def test_public_profile_requires_authentication(client: httpx.AsyncClient) -> None:
    subject = await register_account(client)

    response = await client.get(f"/api/users/{subject.id}")

    assert response.status_code == 401


async def test_patch_updates_only_the_fields_it_names(client: httpx.AsyncClient) -> None:
    account = await register_account(client)
    before = (await client.get("/api/users/me", headers=account.auth)).json()

    response = await client.patch(
        "/api/users/me", json={"bio": "New bio."}, headers=account.auth
    )

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["bio"] == "New bio."
    assert body["full_name"] == before["full_name"]
    assert body["email"] == before["email"]


async def test_patch_with_an_explicit_null_clears_the_field(
    client: httpx.AsyncClient,
) -> None:
    account = await register_account(client)
    assert (
        await client.patch("/api/users/me", json={"bio": "Something"}, headers=account.auth)
    ).status_code == 200

    response = await client.patch("/api/users/me", json={"bio": None}, headers=account.auth)

    assert response.status_code == 200, response.text
    # Absence means "leave it alone", an explicit null means "clear it". A
    # PATCH that could not express the second would leave a user unable to
    # delete their own bio.
    assert response.json()["bio"] is None


async def test_patch_rejects_a_null_full_name(client: httpx.AsyncClient) -> None:
    account = await register_account(client)

    response = await client.patch(
        "/api/users/me", json={"full_name": None}, headers=account.auth
    )

    assert response.status_code == 422
    assert response.json()["error"]["code"] == "validation_error"


@pytest.mark.parametrize(
    "body",
    [
        {"full_name": "   "},
        {"full_name": "x" * 65},
        {"bio": "x" * 501},
        {"phone": "not a phone number"},
        {"phone": "01234567890"},
        {"photo_url": "javascript:alert(1)"},
        {"photo_url": "/relative/path.jpg"},
    ],
)
async def test_patch_rejects_invalid_values(client: httpx.AsyncClient, body: dict) -> None:
    account = await register_account(client)

    response = await client.patch("/api/users/me", json=body, headers=account.auth)

    assert response.status_code == 422, response.text


async def test_empty_patch_is_a_no_op(client: httpx.AsyncClient, session: AsyncSession) -> None:
    account = await register_account(client)

    response = await client.patch("/api/users/me", json={}, headers=account.auth)

    assert response.status_code == 200, response.text
    assert await _profile_events(session) == []


# --- the event boundary -----------------------------------------------------


async def test_changing_full_name_stages_a_profile_updated_event(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    account = await register_account(client)

    response = await client.patch(
        "/api/users/me", json={"full_name": "Alex K."}, headers=account.auth
    )
    assert response.status_code == 200, response.text

    events = await _profile_events(session)
    assert len(events) == 1
    row = events[0]
    assert str(row.aggregate_id) == account.id
    assert row.payload == {
        "user_id": account.id,
        "display_name": "Alex K.",
        "avatar_url": None,
        # The payload is the full current profile, not a delta — that is what
        # contracts/events.md specifies — even though a bio change on its own
        # would not have published anything.
        "bio": "Weekend hiker, slow walker, good with maps.",
        "updated_at": response.json()["updated_at"].replace("+00:00", "Z"),
    }
    assert row.published_at is None


async def test_changing_photo_url_stages_a_profile_updated_event(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    account = await register_account(client)

    response = await client.patch(
        "/api/users/me",
        json={"photo_url": "https://cdn.example.com/a/alex.jpg"},
        headers=account.auth,
    )
    assert response.status_code == 200, response.text

    events = await _profile_events(session)
    assert len(events) == 1
    assert events[0].payload["avatar_url"] == "https://cdn.example.com/a/alex.jpg"
    # Not normalised on the way through: what the caller sent is what is
    # stored and what is published.
    assert response.json()["photo_url"] == "https://cdn.example.com/a/alex.jpg"


async def test_bio_and_phone_changes_publish_nothing(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    account = await register_account(client)

    response = await client.patch(
        "/api/users/me",
        json={"bio": "Now a birdwatcher.", "phone": "+441234567890"},
        headers=account.auth,
    )
    assert response.status_code == 200, response.text

    # The trip service projects display name and avatar only. An event for a
    # bio edit would be one every consumer has to receive and then decide to
    # ignore.
    assert await _profile_events(session) == []


async def test_setting_a_field_to_its_current_value_publishes_nothing(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    account = await register_account(client)
    current = (await client.get("/api/users/me", headers=account.auth)).json()

    response = await client.patch(
        "/api/users/me", json={"full_name": current["full_name"]}, headers=account.auth
    )

    assert response.status_code == 200, response.text
    assert await _profile_events(session) == []


async def test_the_profile_row_and_the_event_agree_on_updated_at(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    account = await register_account(client)

    response = await client.patch(
        "/api/users/me", json={"full_name": "Alex K."}, headers=account.auth
    )
    assert response.status_code == 200, response.text

    stored = await session.execute(
        text("SELECT updated_at FROM users WHERE id = :id"), {"id": account.id}
    )
    updated_at = stored.scalar_one()
    events = await _profile_events(session)
    # Consumers discard out-of-order updates by comparing this timestamp; a
    # row and an event that disagreed about it would make that a coin flip.
    assert events[0].payload["updated_at"] == (
        updated_at.astimezone(dt.UTC).isoformat().replace("+00:00", "Z")
    )


async def _profile_events(session: AsyncSession) -> list[Outbox]:
    result = await session.execute(
        select(Outbox).where(Outbox.event_type == "user.profile_updated")
    )
    return list(result.scalars())
