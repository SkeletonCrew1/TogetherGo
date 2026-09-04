"""GET /internal/users — the shape other services are built against.

Everything asserted here is contract. Changing one of these assertions means
changing `contracts/openapi/identity.yaml` and telling the trip service.
"""

from __future__ import annotations

import uuid

import httpx

from tests.conftest import INTERNAL_TOKEN, register_account

INTERNAL_HEADERS = {"X-Internal-Token": INTERNAL_TOKEN}


async def test_resolves_a_batch_of_ids(client: httpx.AsyncClient) -> None:
    first = await register_account(client, full_name="Alex Traveller")
    second = await register_account(client, full_name="Sam Walker")

    response = await client.get(
        "/internal/users",
        params={"ids": f"{first.id},{second.id}"},
        headers=INTERNAL_HEADERS,
    )

    assert response.status_code == 200, response.text
    users = {user["id"]: user for user in response.json()["users"]}
    assert set(users) == {first.id, second.id}
    assert users[first.id]["full_name"] == "Alex Traveller"


async def test_the_response_carries_exactly_five_fields(client: httpx.AsyncClient) -> None:
    account = await register_account(client)

    response = await client.get(
        "/internal/users", params={"ids": account.id}, headers=INTERNAL_HEADERS
    )

    assert response.status_code == 200, response.text
    (user,) = response.json()["users"]
    # Small and stable. Anything added here is added to
    # contracts/openapi/identity.yaml in the same change.
    assert set(user) == {"id", "full_name", "photo_url", "rating_avg", "rating_count"}
    assert account.email not in response.text


async def test_without_the_token_it_is_401(client: httpx.AsyncClient) -> None:
    account = await register_account(client)

    response = await client.get("/internal/users", params={"ids": account.id})

    assert response.status_code == 401
    assert response.json()["error"]["code"] == "invalid_internal_token"


async def test_with_the_wrong_token_it_is_401(client: httpx.AsyncClient) -> None:
    account = await register_account(client)

    response = await client.get(
        "/internal/users",
        params={"ids": account.id},
        headers={"X-Internal-Token": "not-the-internal-token-at-all"},
    )

    assert response.status_code == 401
    assert response.json()["error"]["code"] == "invalid_internal_token"


async def test_a_user_access_token_does_not_open_the_internal_route(
    client: httpx.AsyncClient,
) -> None:
    account = await register_account(client)

    response = await client.get(
        "/internal/users", params={"ids": account.id}, headers=account.auth
    )

    # The two authentication schemes are separate on purpose: an end user's
    # bearer token must never resolve arbitrary ids in bulk.
    assert response.status_code == 401


async def test_one_hundred_ids_is_allowed(client: httpx.AsyncClient) -> None:
    account = await register_account(client)
    ids = [account.id] + [str(uuid.uuid4()) for _ in range(99)]

    response = await client.get(
        "/internal/users", params={"ids": ",".join(ids)}, headers=INTERNAL_HEADERS
    )

    assert response.status_code == 200, response.text
    assert len(response.json()["users"]) == 1


async def test_one_hundred_and_one_ids_is_400(client: httpx.AsyncClient) -> None:
    ids = [str(uuid.uuid4()) for _ in range(101)]

    response = await client.get(
        "/internal/users", params={"ids": ",".join(ids)}, headers=INTERNAL_HEADERS
    )

    assert response.status_code == 400
    body = response.json()
    assert body["error"]["code"] == "too_many_ids"
    # Truncating to 100 instead would give the caller placeholders for the
    # other id and no way to find out why.
    assert body["error"]["details"] == {"limit": 100, "received": 101}


async def test_the_limit_counts_ids_as_sent_not_after_deduplication(
    client: httpx.AsyncClient,
) -> None:
    duplicate = str(uuid.uuid4())
    ids = [duplicate] * 101

    response = await client.get(
        "/internal/users", params={"ids": ",".join(ids)}, headers=INTERNAL_HEADERS
    )

    assert response.status_code == 400
    assert response.json()["error"]["code"] == "too_many_ids"


async def test_unknown_ids_are_omitted_rather_than_failing_the_batch(
    client: httpx.AsyncClient,
) -> None:
    account = await register_account(client)
    missing = str(uuid.uuid4())

    response = await client.get(
        "/internal/users",
        params={"ids": f"{missing},{account.id}"},
        headers=INTERNAL_HEADERS,
    )

    assert response.status_code == 200, response.text
    # A trip whose member deleted their account still has to render. The
    # caller shows a placeholder for the id it did not get back.
    assert [user["id"] for user in response.json()["users"]] == [account.id]


async def test_all_ids_unknown_returns_an_empty_list(client: httpx.AsyncClient) -> None:
    response = await client.get(
        "/internal/users", params={"ids": str(uuid.uuid4())}, headers=INTERNAL_HEADERS
    )

    assert response.status_code == 200, response.text
    assert response.json() == {"users": []}


async def test_a_malformed_id_is_400(client: httpx.AsyncClient) -> None:
    response = await client.get(
        "/internal/users", params={"ids": "not-a-uuid"}, headers=INTERNAL_HEADERS
    )

    assert response.status_code == 400
    assert response.json()["error"]["code"] == "invalid_ids"


async def test_an_empty_ids_parameter_is_400(client: httpx.AsyncClient) -> None:
    response = await client.get(
        "/internal/users", params={"ids": " , "}, headers=INTERNAL_HEADERS
    )

    assert response.status_code == 400
    assert response.json()["error"]["code"] == "invalid_ids"


async def test_the_documented_cache_ttl_is_on_the_response(
    client: httpx.AsyncClient,
) -> None:
    account = await register_account(client)

    response = await client.get(
        "/internal/users", params={"ids": account.id}, headers=INTERNAL_HEADERS
    )

    # The TTL callers align their own caches with. `private` because the
    # response is per-caller and must not be held by a shared proxy.
    assert response.headers["cache-control"] == "private, max-age=60"


async def test_a_deactivated_account_is_still_resolvable(
    client: httpx.AsyncClient, app
) -> None:
    from sqlalchemy import text

    account = await register_account(client)
    async with app.state.engine.begin() as connection:
        await connection.execute(
            text("UPDATE users SET is_active = false WHERE id = :id"), {"id": account.id}
        )

    response = await client.get(
        "/internal/users", params={"ids": account.id}, headers=INTERNAL_HEADERS
    )

    assert response.status_code == 200, response.text
    # is_active governs signing in. The row still exists, and a trip they are
    # on should still be able to show who they are.
    assert [user["id"] for user in response.json()["users"]] == [account.id]
