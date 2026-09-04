"""Registration: validation, conflict handling, and the outbox write."""

from __future__ import annotations

import httpx
import pytest
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from app.models import Outbox, User
from tests.conftest import register, registration_payload


async def test_register_returns_the_account_and_a_token_pair(client: httpx.AsyncClient) -> None:
    payload = registration_payload()
    response = await client.post("/api/auth/register", json=payload)

    assert response.status_code == 201, response.text
    body = response.json()

    assert body["user"]["email"] == payload["email"]
    assert body["user"]["full_name"] == payload["full_name"]
    assert body["user"]["is_active"] is True
    assert body["expires_in"] == 900
    assert body["access_token"]
    assert body["refresh_token"]

    # Nothing credential-shaped is ever echoed back.
    assert "password" not in body["user"]
    assert "password_hash" not in body["user"]


async def test_duplicate_email_is_a_generic_conflict(client: httpx.AsyncClient) -> None:
    payload = registration_payload()
    assert (await client.post("/api/auth/register", json=payload)).status_code == 201

    duplicate = await client.post("/api/auth/register", json=payload)

    assert duplicate.status_code == 409
    error = duplicate.json()["error"]
    assert error["code"] == "registration_conflict"
    # The message must not confirm that the address is registered.
    assert "email" not in error["message"].lower()
    assert "exist" not in error["message"].lower()
    assert "taken" not in error["message"].lower()


async def test_email_uniqueness_ignores_case(client: httpx.AsyncClient) -> None:
    payload = registration_payload(email="Alex@Example.com")
    assert (await client.post("/api/auth/register", json=payload)).status_code == 201

    other_case = await client.post(
        "/api/auth/register", json=registration_payload(email="alex@EXAMPLE.com")
    )

    assert other_case.status_code == 409


async def test_registration_stages_user_registered_in_the_outbox(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    response = await register(client)
    user_id = response.json()["user"]["id"]

    rows = list((await session.execute(select(Outbox))).scalars())

    assert len(rows) == 1
    row = rows[0]
    assert row.event_type == "user.registered"
    assert str(row.aggregate_id) == user_id
    assert row.published_at is None
    assert row.payload["user_id"] == user_id
    assert row.payload["display_name"] == "Alex Traveller"
    assert row.payload["verification_token"]
    assert row.payload["registered_at"].endswith("Z")


async def test_a_rejected_registration_leaves_no_outbox_row(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """The domain write and the event are one transaction, so both roll back."""
    payload = registration_payload()
    assert (await client.post("/api/auth/register", json=payload)).status_code == 201
    assert (await client.post("/api/auth/register", json=payload)).status_code == 409

    users = list((await session.execute(select(User))).scalars())
    events = list((await session.execute(select(Outbox))).scalars())

    assert len(users) == 1
    assert len(events) == 1


@pytest.mark.parametrize(
    ("password", "reason"),
    [
        ("short1234", "shorter than ten characters"),
        ("1234567890123", "entirely numeric"),
        ("traveller-secret-99", "contains the email local part"),
    ],
)
async def test_weak_passwords_are_rejected(
    client: httpx.AsyncClient, password: str, reason: str
) -> None:
    response = await client.post(
        "/api/auth/register",
        json=registration_payload(email="traveller@example.com", password=password),
    )

    assert response.status_code == 422, reason
    body = response.json()
    assert body["error"]["code"] == "validation_error"
    assert any(field["field"] == "password" for field in body["error"]["details"]["fields"])
    # The rejected password must not come back in the error body.
    assert password not in response.text


async def test_under_sixteen_is_rejected(client: httpx.AsyncClient) -> None:
    import datetime as dt

    # UTC, because that is the clock the validator reads. Using the local
    # date would put this test a day out of step west of Greenwich late in
    # the evening — a flake that only appears on some machines.
    almost_sixteen = dt.datetime.now(dt.UTC).date() - dt.timedelta(days=365 * 15)
    response = await client.post(
        "/api/auth/register",
        json=registration_payload(birth_date=almost_sixteen.isoformat()),
    )

    assert response.status_code == 422
    fields = response.json()["error"]["details"]["fields"]
    assert any(field["field"] == "birth_date" for field in fields)


async def test_exactly_sixteen_is_accepted(client: httpx.AsyncClient) -> None:
    import datetime as dt

    today = dt.datetime.now(dt.UTC).date()
    try:
        sixteen_today = today.replace(year=today.year - 16)
    except ValueError:  # 29 February in a year whose -16 is not a leap year
        sixteen_today = today.replace(year=today.year - 16, day=28)
    response = await client.post(
        "/api/auth/register",
        json=registration_payload(birth_date=sixteen_today.isoformat()),
    )

    assert response.status_code == 201, response.text
