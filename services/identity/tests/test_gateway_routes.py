"""The gateway routing table, checked against the routes this service exposes.

`deploy/traefik/dynamic.yml` is configuration, so nothing type-checks it and
nothing fails at build time when it drifts. The one rule in it that is a
security boundary rather than a convenience — that `/internal` is not routed
through the gateway — is therefore asserted here.

Since the SPA moved into the stack the table has a catch-all router (`web`,
`PathPrefix(`/`)`), so "is this path routed at all" is no longer the same
question as "does this path reach a service". These tests ask the second one:
which router wins, and what it forwards to.
"""

from __future__ import annotations

import re

import pytest
import yaml
from fastapi import FastAPI

from tests.conftest import REPO_ROOT

DYNAMIC_CONFIG = REPO_ROOT / "deploy" / "traefik" / "dynamic.yml"

# Traefik v3 rules: PathPrefix(`/api/auth`), joined with || and &&, and
# optionally negated with a leading `!` — which the catch-all uses to keep
# /internal out.
_PATH_PREFIX = re.compile(r"(!\s*)?PathPrefix\(`([^`]+)`\)")


@pytest.fixture(scope="module")
def routing_table() -> dict:
    return yaml.safe_load(DYNAMIC_CONFIG.read_text())


def _covers(prefix: str, path: str) -> bool:
    """Whether Traefik's PathPrefix(`prefix`) matches `path`."""
    # `/` is the catch-all and matches everything, including `/`.
    if prefix == "/":
        return True
    return path == prefix or path.startswith(prefix + "/")


def router_prefixes(rule: str) -> tuple[list[str], list[str]]:
    """A rule's prefixes, split into the ones it accepts and the ones it excludes."""
    accepted: list[str] = []
    excluded: list[str] = []
    for negated, prefix in _PATH_PREFIX.findall(rule):
        (excluded if negated else accepted).append(prefix)
    return accepted, excluded


def routers_for(routing_table: dict, path: str) -> list[str]:
    """Every router whose rule would accept `path`, by name.

    Priority is not modelled: what these tests care about is whether a path
    reaches a *service*, and a path matched by two routers reaches one of them
    either way. The priorities themselves are asserted separately, below.
    """
    matched: list[str] = []
    for name, router in routing_table["http"]["routers"].items():
        accepted, excluded = router_prefixes(router["rule"])
        if any(_covers(p, path) for p in accepted) and not any(
            _covers(p, path) for p in excluded
        ):
            matched.append(name)
    return matched


def reaches_the_gateway(routing_table: dict, path: str) -> bool:
    return bool(routers_for(routing_table, path))


def reaches_identity(routing_table: dict, path: str) -> bool:
    return "identity" in routers_for(routing_table, path)


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
    accepted, _ = router_prefixes(rule)

    assert {"/api/auth", "/api/users", "/.well-known"} <= set(accepted)
    assert routing_table["http"]["routers"]["identity"]["service"] == "identity"
    assert (
        routing_table["http"]["services"]["identity"]["loadBalancer"]["servers"][0]["url"]
        == "http://identity:8001"
    )


def test_internal_is_not_routed_through_the_gateway(routing_table: dict) -> None:
    # The acceptance criterion, stated as a test: curling
    # http://localhost:8080/internal/users must be a 404 from Traefik, not a
    # 401 from this application and not the SPA's index.html. No router
    # accepts it — the API routers do not match the prefix, and the catch-all
    # excludes it explicitly — so Traefik has nowhere to send it and answers
    # 404 itself. The X-Internal-Token check is the second line of defence
    # behind this, not the first.
    assert not reaches_the_gateway(routing_table, "/internal/users")
    assert not reaches_the_gateway(routing_table, "/internal")


def test_the_catch_all_excludes_internal_rather_than_swallowing_it(
    routing_table: dict,
) -> None:
    # The catch-all exists to serve the SPA, and a bare PathPrefix(`/`) would
    # match /internal and forward it to the frontend. That is not a leak of
    # identity's data — the request never reaches identity either way — but it
    # would silently downgrade "404 from Traefik" to "200 with an HTML page",
    # and the test above would stop meaning what it says. Assert the exclusion
    # is really in the rule, so deleting it fails here with a clear reason.
    web = routing_table["http"]["routers"]["web"]
    accepted, excluded = router_prefixes(web["rule"])

    assert accepted == ["/"], "the SPA router is the catch-all"
    assert "/internal" in excluded
    assert web["service"] == "web"


def test_the_catch_all_cannot_outrank_an_api_router(routing_table: dict) -> None:
    # Traefik falls back to the rule's *length* when no priority is given, so
    # without an explicit one the question "does /api/trips reach trip or the
    # SPA" would be decided by character counts and could flip on a rename.
    # The symptom is the confusing kind: the page loads and every API call
    # comes back as index.html.
    routers = routing_table["http"]["routers"]
    web_priority = routers["web"]["priority"]

    assert web_priority >= 1, "Traefik rejects a priority below 1"
    for name, router in routers.items():
        if name == "web":
            continue
        # An API router either states a higher priority or has none at all, in
        # which case its length-derived priority is far above 1.
        assert router.get("priority", len(router["rule"])) > web_priority, name


def test_every_internal_route_this_service_exposes_is_unroutable(
    app: FastAPI, routing_table: dict
) -> None:
    internal_paths = [path for path in service_paths(app) if path.startswith("/internal")]

    # If a later change adds a second /internal endpoint, this catches a
    # routing table that was never updated to keep excluding it.
    assert internal_paths, "expected at least one /internal route to guard"
    assert [
        path for path in internal_paths if reaches_the_gateway(routing_table, path)
    ] == []


def test_every_public_route_this_service_exposes_is_routable(
    app: FastAPI, routing_table: dict
) -> None:
    # Reaches *identity*, not merely "reaches the gateway": with a catch-all in
    # the table every path reaches something, and a public endpoint that fell
    # through to the SPA would answer 200 with an HTML page rather than 404 —
    # a missing route that looks like a working one.
    #
    # Health endpoints are probed by Traefik and by Compose over the internal
    # network, not by clients, so they are deliberately not routed.
    unrouted = {"/healthz", "/readyz"}
    public_paths = [
        path
        for path in service_paths(app)
        if not path.startswith("/internal") and path not in unrouted
    ]

    assert [
        path for path in public_paths if not reaches_identity(routing_table, path)
    ] == []
