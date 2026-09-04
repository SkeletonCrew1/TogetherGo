from __future__ import annotations

import httpx


async def test_healthz_checks_nothing_but_the_process(client: httpx.AsyncClient) -> None:
    response = await client.get("/healthz")

    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


async def test_readyz_reports_the_database(client: httpx.AsyncClient) -> None:
    response = await client.get("/readyz")

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["status"] == "ready"
    assert body["checks"]["database"] == "ok"
    # The relay is off in tests, so there is no broker dependency to report.
    assert body["checks"]["broker"] == "disabled"


async def test_every_response_carries_a_request_id(client: httpx.AsyncClient) -> None:
    response = await client.get("/healthz")
    assert response.headers["x-request-id"]

    echoed = await client.get("/healthz", headers={"X-Request-ID": "abc-123"})
    assert echoed.headers["x-request-id"] == "abc-123"
