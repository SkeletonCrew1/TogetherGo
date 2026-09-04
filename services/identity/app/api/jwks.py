"""GET /.well-known/jwks.json — the public half of the signing key."""

from __future__ import annotations

from typing import Any

from fastapi import APIRouter, Depends, Response

from app.deps import get_issuer
from app.services.tokens import TokenIssuer

router = APIRouter(tags=["jwks"])


@router.get(
    "/.well-known/jwks.json",
    summary="RS256 public key as a JWKS document",
)
async def jwks(response: Response, issuer: TokenIssuer = Depends(get_issuer)) -> dict[str, Any]:
    # Every other service caches this for 10 minutes and refetches on an
    # unknown `kid` (CLAUDE.md), so the cache header matches that policy
    # rather than inventing a second one.
    response.headers["Cache-Control"] = "public, max-age=600"
    return issuer.jwks
