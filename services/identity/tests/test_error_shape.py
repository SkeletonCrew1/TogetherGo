"""The declared error model and the one the handlers actually emit are one shape.

`app.errors.install_handlers` builds error bodies by hand — it answers
exceptions, so it cannot return a response model — while `ErrorResponse` is
what the OpenAPI document promises. Nothing but this test keeps the two from
drifting apart.
"""

from __future__ import annotations

import uuid

import httpx
import pytest

from app.schemas import ErrorResponse
from tests.conftest import INTERNAL_TOKEN, register_account


@pytest.mark.parametrize(
    "case",
    [
        "unauthenticated",
        "not_found",
        "bad_internal_token",
        "too_many_ids",
        "validation_error",
    ],
)
async def test_every_error_body_matches_the_declared_model(
    client: httpx.AsyncClient, case: str
) -> None:
    account = await register_account(client)

    responses = {
        "unauthenticated": lambda: client.get("/api/users/me"),
        "not_found": lambda: client.get(
            f"/api/users/{uuid.uuid4()}", headers=account.auth
        ),
        "bad_internal_token": lambda: client.get(
            "/internal/users",
            params={"ids": account.id},
            headers={"X-Internal-Token": "wrong"},
        ),
        "too_many_ids": lambda: client.get(
            "/internal/users",
            params={"ids": ",".join(str(uuid.uuid4()) for _ in range(101))},
            headers={"X-Internal-Token": INTERNAL_TOKEN},
        ),
        "validation_error": lambda: client.patch(
            "/api/users/me", json={"full_name": ""}, headers=account.auth
        ),
    }

    response = await responses[case]()

    assert response.status_code >= 400
    # Raises if the body is anything other than the documented shape.
    parsed = ErrorResponse.model_validate(response.json())
    assert parsed.error.code
    assert parsed.error.message
