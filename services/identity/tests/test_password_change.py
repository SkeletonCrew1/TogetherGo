"""POST /api/users/me/password.

The interesting property is the asymmetry: every *other* session dies, and
the caller's survives. Getting that backwards in either direction is a real
bug — revoking nothing leaves a thief's refresh token alive, revoking
everything logs the user out of the tab they just used.
"""

from __future__ import annotations

import httpx

from tests.conftest import login, register_account


async def _refresh_works(client: httpx.AsyncClient, refresh_token: str) -> bool:
    response = await client.post("/api/auth/refresh", json={"refresh_token": refresh_token})
    return response.status_code == 200


async def test_password_change_revokes_other_sessions_but_not_the_current_one(
    client: httpx.AsyncClient,
) -> None:
    laptop = await register_account(client)
    phone = await login(client, laptop)
    tablet = await login(client, laptop)

    response = await client.post(
        "/api/users/me/password",
        json={"current_password": laptop.password, "new_password": "a much better secret"},
        headers=laptop.auth,
    )

    assert response.status_code == 204, response.text
    # The caller's own session survives: it is identified through the `jti`
    # of the access token it presented, which the refresh token row records.
    assert await _refresh_works(client, laptop.refresh_token)
    assert not await _refresh_works(client, phone.refresh_token)
    assert not await _refresh_works(client, tablet.refresh_token)


async def test_the_current_session_survives_a_refresh_before_the_change(
    client: httpx.AsyncClient,
) -> None:
    laptop = await register_account(client)
    other = await login(client, laptop)

    # The client rotates, then changes its password still holding the access
    # token from before the rotation — it is valid for another fifteen
    # minutes. The row that `jti` points at has been rotated away by now, so
    # sparing that row alone would not be enough; the whole chain is spared.
    rotated = await client.post(
        "/api/auth/refresh", json={"refresh_token": laptop.refresh_token}
    )
    assert rotated.status_code == 200, rotated.text
    new_refresh_token = rotated.json()["refresh_token"]

    response = await client.post(
        "/api/users/me/password",
        json={"current_password": laptop.password, "new_password": "a much better secret"},
        headers=laptop.auth,
    )

    assert response.status_code == 204, response.text
    assert await _refresh_works(client, new_refresh_token)
    assert not await _refresh_works(client, other.refresh_token)


async def test_the_new_password_is_the_one_that_works(client: httpx.AsyncClient) -> None:
    account = await register_account(client)
    new_password = "a much better secret"

    assert (
        await client.post(
            "/api/users/me/password",
            json={"current_password": account.password, "new_password": new_password},
            headers=account.auth,
        )
    ).status_code == 204

    old = await client.post(
        "/api/auth/login", json={"email": account.email, "password": account.password}
    )
    new = await client.post(
        "/api/auth/login", json={"email": account.email, "password": new_password}
    )

    assert old.status_code == 401
    assert new.status_code == 200, new.text


async def test_a_wrong_current_password_changes_nothing(client: httpx.AsyncClient) -> None:
    account = await register_account(client)
    other = await login(client, account)

    response = await client.post(
        "/api/users/me/password",
        json={"current_password": "not the password", "new_password": "a much better secret"},
        headers=account.auth,
    )

    # 403, not 401: the access token is fine and the caller is authenticated.
    # A 401 would tell every client library to throw the session away and
    # refresh, which is the wrong reaction to a typo in a form field.
    assert response.status_code == 403
    assert response.json()["error"]["code"] == "invalid_current_password"
    # A failed attempt must not be a way to sign somebody else's sessions out.
    assert await _refresh_works(client, other.refresh_token)


async def test_it_requires_a_bearer_token(client: httpx.AsyncClient) -> None:
    account = await register_account(client)

    response = await client.post(
        "/api/users/me/password",
        json={"current_password": account.password, "new_password": "a much better secret"},
    )

    assert response.status_code == 401


async def test_the_new_password_faces_the_registration_rules(
    client: httpx.AsyncClient,
) -> None:
    account = await register_account(client)

    too_short = await client.post(
        "/api/users/me/password",
        json={"current_password": account.password, "new_password": "short"},
        headers=account.auth,
    )
    all_digits = await client.post(
        "/api/users/me/password",
        json={"current_password": account.password, "new_password": "1234567890123"},
        headers=account.auth,
    )
    unchanged = await client.post(
        "/api/users/me/password",
        json={"current_password": account.password, "new_password": account.password},
        headers=account.auth,
    )

    assert too_short.status_code == 422
    assert all_digits.status_code == 422
    assert unchanged.status_code == 422


async def test_the_new_password_may_not_embed_the_email_local_part(
    client: httpx.AsyncClient,
) -> None:
    account = await register_account(client, email="birdwatcher@example.com")

    response = await client.post(
        "/api/users/me/password",
        json={
            "current_password": account.password,
            "new_password": "birdwatcher forever",
        },
        headers=account.auth,
    )

    # Registration enforces this rule too. A rule applied at only one of the
    # two is a rule a user routes around by signing up and then changing.
    assert response.status_code == 422
    assert response.json()["error"]["code"] == "weak_password"
