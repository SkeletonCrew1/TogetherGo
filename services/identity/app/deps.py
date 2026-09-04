"""FastAPI dependencies — the only place routers reach into app state."""

from __future__ import annotations

import secrets
import uuid
from collections.abc import AsyncIterator
from typing import Annotated

from fastapi import Depends, Query, Request
from fastapi.security import APIKeyHeader, HTTPAuthorizationCredentials, HTTPBearer
from sqlalchemy.ext.asyncio import AsyncSession

from app import logging as app_logging
from app.errors import InvalidAccessToken, InvalidInternalToken, RateLimited
from app.models import User
from app.schemas.internal import InternalUsersQuery
from app.services.auth_service import AuthService
from app.services.profile_service import ProfileService
from app.services.rate_limit import RateLimiter
from app.services.rating_service import RatingService
from app.services.tokens import AccessTokenClaims, TokenIssuer

INTERNAL_TOKEN_HEADER = "X-Internal-Token"


async def get_session(request: Request) -> AsyncIterator[AsyncSession]:
    async with request.app.state.sessionmaker() as session:
        yield session


def get_issuer(request: Request) -> TokenIssuer:
    return request.app.state.issuer


def get_auth_service(
    request: Request,
    session: AsyncSession = Depends(get_session),
) -> AuthService:
    return AuthService(
        session,
        issuer=request.app.state.issuer,
        refresh_token_ttl=request.app.state.settings.refresh_token_ttl,
    )


def get_profile_service(session: AsyncSession = Depends(get_session)) -> ProfileService:
    return ProfileService(session)


def get_rating_service(session: AsyncSession = Depends(get_session)) -> RatingService:
    return RatingService(session)


# auto_error=False so a missing or malformed Authorization header lands on our
# own error shape instead of Starlette's `{"detail": ...}`. Declaring the
# scheme at all is what puts the padlock on /docs and `bearerAuth` in the
# generated OpenAPI document.
_bearer = HTTPBearer(auto_error=False, description="RS256 access token")


async def access_token_claims(
    request: Request,
    credentials: HTTPAuthorizationCredentials | None = Depends(_bearer),
) -> AccessTokenClaims:
    """Verify the bearer token and return its claims.

    Declared async so it runs on the event loop: FastAPI hands a `def`
    dependency to a worker thread, and the `user_id_var` set below would be
    made in that thread's copy of the context and lost on the way back.

    Identity verifies with its own signing key — it is the issuer. Every
    other service fetches the JWKS instead; none of them, and not this one,
    trusts a gateway-injected identity header (CLAUDE.md rule 6).
    """
    if credentials is None or not credentials.credentials:
        raise InvalidAccessToken()

    issuer: TokenIssuer = request.app.state.issuer
    claims = issuer.verify_access_token(credentials.credentials)
    # Every log line for the rest of this request carries the user id, with
    # nothing having to thread it through by hand.
    app_logging.user_id_var.set(str(claims.user_id))
    return claims


async def current_user(
    claims: AccessTokenClaims = Depends(access_token_claims),
    service: ProfileService = Depends(get_profile_service),
) -> User:
    """The authenticated account, loaded fresh on every request.

    A token outlives the facts it was minted from: an account deactivated
    four minutes ago still has valid access tokens in the wild. The row is
    the authority on whether the caller may act, not the claim set.
    """
    user = await service.get_profile(claims.user_id)
    if not user.is_active:
        raise InvalidAccessToken()
    return user


# Declared as a security scheme, not read straight off the request, so the
# generated OpenAPI document tells the trip service which header to send.
_internal_token = APIKeyHeader(
    name=INTERNAL_TOKEN_HEADER,
    auto_error=False,
    description="Shared secret; must equal INTERNAL_API_TOKEN.",
)


def require_internal_token(
    request: Request,
    presented: str | None = Depends(_internal_token),
) -> None:
    """Gate for /internal routes.

    A second line of defence, not the first: /internal is not routed through
    the gateway at all (see deploy/traefik/dynamic.yml), so anything reaching
    here is already on the Compose network. The comparison is constant-time
    because a byte-at-a-time one is a usable oracle for the token.
    """
    expected: str = request.app.state.settings.internal_api_token
    if presented is None or not secrets.compare_digest(presented, expected):
        raise InvalidInternalToken()


def internal_user_ids(
    query: Annotated[InternalUsersQuery, Query()],
) -> list[uuid.UUID]:
    """Parse `?ids=a,b,c` into ids, or raise the contract's 400s."""
    return query.user_ids()


def client_ip(request: Request) -> str:
    """The address the rate limiter buckets on.

    Traefik *appends* the peer it saw to X-Forwarded-For, so the rightmost
    entry is the one our own gateway observed and the only one a client
    cannot forge. Taking the leftmost — the usual reflex — would let any
    caller reset its own bucket by sending a made-up header.
    """
    forwarded = request.headers.get("x-forwarded-for")
    if forwarded:
        parts = [part.strip() for part in forwarded.split(",") if part.strip()]
        if parts:
            return parts[-1]
    return request.client.host if request.client else "unknown"


class RateLimit:
    """Dependency that spends one attempt from an IP's budget."""

    def __init__(self, scope: str) -> None:
        self._scope = scope

    async def __call__(self, request: Request) -> None:
        limiter: RateLimiter | None = getattr(request.app.state, "rate_limiter", None)
        if limiter is None:
            return
        retry_after = await limiter.check(self._scope, client_ip(request))
        if retry_after is not None:
            raise RateLimited(retry_after)


rate_limit_register = RateLimit("register")
rate_limit_login = RateLimit("login")
