"""The gateway routing table, checked against the routes this service exposes.

`deploy/traefik/dynamic.yml` is configuration, so nothing type-checks it and
nothing fails at build time when it drifts. The one rule in it that is a
security boundary rather than a convenience — that `/internal` is not routed
through the gateway — is therefore asserted here.
"""

from __future__ import annotations

import re

import pytest
import yaml
from fastapi import FastAPI

from tests.conftest import REPO_ROOT

DYNAMIC_CONFIG = REPO_ROOT / "deploy" / "traefik" / "dynamic.yml"

# Traefik v3 rules: PathPrefix(`/api/auth`), joined with || and &&.
_PATH_PREFIX = re.compile(r"PathPrefix\(`([^`]+)`\)")


@pytest.fixture(scope="module")
def routing_table() -> dict:
    return yaml.safe_load(DYNAMIC_CONFIG.read_text())


def gateway_prefixes(routing_table: dict) -> list[str]:
    """Every path prefix any router in the file would accept."""
    prefixes: list[str] = []
    for router in routing_table["http"]["routers"].values():
        prefixes.extend(_PATH_PREFIX.findall(router["rule"]))
    return prefixes


def reaches_the_gateway(path: str, prefixes: list[str]) -> bool:
    return any(path == prefix or path.startswith(prefix + "/") for prefix in prefixes)


def service_paths(app: FastAPI) -> list[str]:
    """Every path this service publishes, read from its own OpenAPI document.

    The generated document rather than `app.routes`: routes included from a
    router are not a flat list of `APIRoute` objects in every FastAPI
    version, and the document is the surface the service actually claims to
    have anyway.
    """
    return sorted(app.openapi()["paths"])


def test_the_identity_router_carries_the_three_public_prefixes(routing_table: dict) -> None:
    rule = routing_table["http"]["routers"]["identity"]["rule"]
    prefixes = set(_PATH_PREFIX.findall(rule))

    assert {"/api/auth", "/api/users", "/.well-known"} <= prefixes
    assert routing_table["http"]["routers"]["identity"]["service"] == "identity"
    assert (
        routing_table["http"]["services"]["identity"]["loadBalancer"]["servers"][0]["url"]
        == "http://identity:8001"
    )


def test_internal_is_not_routed_through_the_gateway(routing_table: dict) -> None:
    prefixes = gateway_prefixes(routing_table)

    # The acceptance criterion, stated as a test: curling
    # http://localhost:8080/internal/users must be a 404 from Traefik, not a
    # 401 from this application. No router matches it, so Traefik has nowhere
    # to send it and answers 404 itself. The X-Internal-Token check is the
    # second line of defence behind this, not the first.
    assert not reaches_the_gateway("/internal/users", prefixes)
    assert not reaches_the_gateway("/internal", prefixes)
    # And no prefix is so broad that it would swallow /internal by accident.
    assert not any("/internal".startswith(prefix) for prefix in prefixes)


def test_every_internal_route_this_service_exposes_is_unroutable(
    app: FastAPI, routing_table: dict
) -> None:
    prefixes = gateway_prefixes(routing_table)

    internal_paths = [path for path in service_paths(app) if path.startswith("/internal")]

    # If a later change adds a second /internal endpoint, this catches a
    # routing table that was never updated to keep excluding it.
    assert internal_paths, "expected at least one /internal route to guard"
    assert [path for path in internal_paths if reaches_the_gateway(path, prefixes)] == []


def test_every_public_route_this_service_exposes_is_routable(
    app: FastAPI, routing_table: dict
) -> None:
    prefixes = gateway_prefixes(routing_table)

    # Health endpoints are probed by Traefik and by Compose over the internal
    # network, not by clients, so they are deliberately not routed.
    unrouted = {"/healthz", "/readyz"}
    public_paths = [
        path
        for path in service_paths(app)
        if not path.startswith("/internal") and path not in unrouted
    ]

    assert [path for path in public_paths if not reaches_the_gateway(path, prefixes)] == []
