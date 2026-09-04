"""Refresh-token rotation, replay detection, and logout."""

from __future__ import annotations

import httpx
from sqlalchemy import select
from sqlalchemy.ext.asyncio import AsyncSession

from app.models import RefreshToken
from tests.conftest import register, registration_payload


async def _login(client: httpx.AsyncClient, payload: dict) -> dict:
    response = await client.post(
        "/api/auth/login", json={"email": payload["email"], "password": payload["password"]}
    )
    assert response.status_code == 200, response.text
    return response.json()


async def test_refresh_rotates_and_retires_the_old_token(client: httpx.AsyncClient) -> None:
    registered = (await register(client)).json()
    old_token = registered["refresh_token"]

    rotated = await client.post("/api/auth/refresh", json={"refresh_token": old_token})

    assert rotated.status_code == 200, rotated.text
    body = rotated.json()
    assert body["refresh_token"] != old_token
    assert body["access_token"]
    assert body["expires_in"] == 900
    # A rotation response is tokens only — no profile.
    assert "user" not in body

    # The successor works.
    again = await client.post("/api/auth/refresh", json={"refresh_token": body["refresh_token"]})
    assert again.status_code == 200


async def test_rotation_links_the_old_row_to_its_successor(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    registered = (await register(client)).json()

    await client.post("/api/auth/refresh", json={"refresh_token": registered["refresh_token"]})

    rows = list((await session.execute(select(RefreshToken))).scalars())
    assert len(rows) == 2

    predecessor = next(row for row in rows if row.revoked_at is not None)
    successor = next(row for row in rows if row.revoked_at is None)
    assert predecessor.replaced_by == successor.id
    assert predecessor.user_agent == "pytest/1.0"


async def test_an_unknown_refresh_token_is_rejected(client: httpx.AsyncClient) -> None:
    response = await client.post("/api/auth/refresh", json={"refresh_token": "not-a-real-token"})

    assert response.status_code == 401
    assert response.json()["error"]["code"] == "invalid_refresh_token"


async def test_refreshing_twice_with_the_same_token_trips_reuse_detection(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    """The acceptance case: register, then refresh twice with the same token."""
    registered = (await register(client)).json()
    stolen = registered["refresh_token"]

    first = await client.post("/api/auth/refresh", json={"refresh_token": stolen})
    assert first.status_code == 200
    successor = first.json()["refresh_token"]

    replayed = await client.post("/api/auth/refresh", json={"refresh_token": stolen})

    assert replayed.status_code == 401
    # Same body as any other bad token: the attacker is not told they were
    # spotted, only that the token stopped working.
    assert replayed.json()["error"]["code"] == "invalid_refresh_token"

    # The successor handed out a moment ago is collateral — that is the point.
    assert (
        await client.post("/api/auth/refresh", json={"refresh_token": successor})
    ).status_code == 401

    rows = list((await session.execute(select(RefreshToken))).scalars())
    assert rows, "expected the tokens to still exist, just revoked"
    assert all(row.revoked_at is not None for row in rows)


async def test_replay_kills_every_session_for_that_user(client: httpx.AsyncClient) -> None:
    payload = registration_payload()
    registered = (await client.post("/api/auth/register", json=payload)).json()
    laptop = registered["refresh_token"]
    phone = (await _login(client, payload))["refresh_token"]
    tablet = (await _login(client, payload))["refresh_token"]

    # The laptop's token is rotated once, then replayed.
    assert (
        await client.post("/api/auth/refresh", json={"refresh_token": laptop})
    ).status_code == 200
    assert (
        await client.post("/api/auth/refresh", json={"refresh_token": laptop})
    ).status_code == 401

    # Every other device is signed out too. The account is presumed
    # compromised, so nothing that was issued before the replay survives.
    for token in (phone, tablet):
        response = await client.post("/api/auth/refresh", json={"refresh_token": token})
        assert response.status_code == 401
        assert response.json()["error"]["code"] == "invalid_refresh_token"


async def test_replay_does_not_touch_other_users(client: httpx.AsyncClient) -> None:
    victim = (await register(client)).json()["refresh_token"]
    bystander = (await register(client)).json()["refresh_token"]

    await client.post("/api/auth/refresh", json={"refresh_token": victim})
    await client.post("/api/auth/refresh", json={"refresh_token": victim})

    still_valid = await client.post("/api/auth/refresh", json={"refresh_token": bystander})
    assert still_valid.status_code == 200


async def test_logout_ends_one_session_and_leaves_the_others(
    client: httpx.AsyncClient,
) -> None:
    payload = registration_payload()
    laptop = (await client.post("/api/auth/register", json=payload)).json()["refresh_token"]
    phone = (await _login(client, payload))["refresh_token"]

    # Rotate the laptop twice, so its chain is three rows long and the token
    # being presented is the newest link rather than the root.
    laptop = (
        await client.post("/api/auth/refresh", json={"refresh_token": laptop})
    ).json()["refresh_token"]
    laptop = (
        await client.post("/api/auth/refresh", json={"refresh_token": laptop})
    ).json()["refresh_token"]

    logged_out = await client.post("/api/auth/logout", json={"refresh_token": laptop})
    assert logged_out.status_code == 204
    assert logged_out.content == b""

    assert (
        await client.post("/api/auth/refresh", json={"refresh_token": laptop})
    ).status_code == 401
    assert (
        await client.post("/api/auth/refresh", json={"refresh_token": phone})
    ).status_code == 200


async def test_logout_revokes_the_whole_chain_not_just_the_leaf(
    client: httpx.AsyncClient, session: AsyncSession
) -> None:
    registered = (await register(client)).json()
    user_id = registered["user"]["id"]

    leaf = (
        await client.post("/api/auth/refresh", json={"refresh_token": registered["refresh_token"]})
    ).json()["refresh_token"]

    assert (await client.post("/api/auth/logout", json={"refresh_token": leaf})).status_code == 204

    rows = list(
        (await session.execute(select(RefreshToken).where(RefreshToken.user_id == user_id)))
        .scalars()
    )
    assert len(rows) == 2
    assert all(row.revoked_at is not None for row in rows)


async def test_logout_with_an_unknown_token_is_still_204(client: httpx.AsyncClient) -> None:
    """Logout is not an existence oracle either."""
    response = await client.post("/api/auth/logout", json={"refresh_token": "never-issued"})

    assert response.status_code == 204
