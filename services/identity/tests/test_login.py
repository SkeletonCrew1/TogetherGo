"""Login: the failure paths must be indistinguishable from each other."""

from __future__ import annotations

import httpx

from tests.conftest import registration_payload


async def test_login_returns_a_fresh_pair(client: httpx.AsyncClient) -> None:
    payload = registration_payload()
    registered = await client.post("/api/auth/register", json=payload)
    assert registered.status_code == 201

    response = await client.post(
        "/api/auth/login",
        json={"email": payload["email"], "password": payload["password"]},
    )

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["user"]["id"] == registered.json()["user"]["id"]
    assert body["expires_in"] == 900
    # A new session, not the one registration handed out.
    assert body["refresh_token"] != registered.json()["refresh_token"]


async def test_login_is_case_insensitive_in_the_email(client: httpx.AsyncClient) -> None:
    payload = registration_payload(email="Alex@Example.com")
    assert (await client.post("/api/auth/register", json=payload)).status_code == 201

    response = await client.post(
        "/api/auth/login",
        json={"email": "ALEX@example.COM", "password": payload["password"]},
    )

    assert response.status_code == 200, response.text


async def test_wrong_password_and_unknown_email_are_indistinguishable(
    client: httpx.AsyncClient,
) -> None:
    payload = registration_payload()
    assert (await client.post("/api/auth/register", json=payload)).status_code == 201

    wrong_password = await client.post(
        "/api/auth/login",
        json={"email": payload["email"], "password": "definitely not the one"},
    )
    unknown_email = await client.post(
        "/api/auth/login",
        json={"email": "nobody-at-all@example.com", "password": payload["password"]},
    )

    assert wrong_password.status_code == 401
    assert unknown_email.status_code == 401
    # Byte-for-byte identical: status, code and message. Anything that differs
    # is an oracle for which addresses are registered.
    assert wrong_password.json() == unknown_email.json()
    assert wrong_password.json()["error"]["code"] == "invalid_credentials"


async def test_unknown_email_still_costs_a_password_verification(
    client: httpx.AsyncClient,
) -> None:
    """Timing must not separate the two failures.

    Argon2id at 64 MiB dominates the response, so the assertion is that the
    unknown-email path is not an order of magnitude faster — not that the two
    are equal, which no wall-clock test can honestly claim.
    """
    import time

    payload = registration_payload()
    assert (await client.post("/api/auth/register", json=payload)).status_code == 201

    async def timed(email: str) -> float:
        started = time.perf_counter()
        await client.post("/api/auth/login", json={"email": email, "password": "wrong password!"})
        return time.perf_counter() - started

    # Warm the interpreter so the first call does not carry import cost.
    await timed(payload["email"])

    known = await timed(payload["email"])
    unknown = await timed("nobody-at-all@example.com")

    assert unknown > known / 3, f"unknown-email path returned suspiciously fast: {unknown} vs {known}"


async def test_rate_limit_kicks_in_after_ten_attempts(client: httpx.AsyncClient) -> None:
    body = {"email": "nobody-at-all@example.com", "password": "wrong password!"}

    for attempt in range(10):
        response = await client.post("/api/auth/login", json=body)
        assert response.status_code == 401, f"attempt {attempt} was already limited"

    limited = await client.post("/api/auth/login", json=body)

    assert limited.status_code == 429
    assert limited.json()["error"]["code"] == "rate_limited"
    retry_after = int(limited.headers["retry-after"])
    assert 0 < retry_after <= 900
