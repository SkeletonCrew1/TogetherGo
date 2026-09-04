"""The JWKS document and the tokens it has to verify."""

from __future__ import annotations

import datetime as dt

import httpx
import jwt
import pytest

from tests.conftest import register


async def test_jwks_document_has_the_expected_shape(client: httpx.AsyncClient) -> None:
    response = await client.get("/.well-known/jwks.json")

    assert response.status_code == 200
    keys = response.json()["keys"]
    assert len(keys) == 1

    key = keys[0]
    assert key["kty"] == "RSA"
    assert key["use"] == "sig"
    assert key["alg"] == "RS256"
    assert key["kid"] == "test-key-1"
    assert key["n"] and key["e"]
    # base64url, unpadded.
    assert "=" not in key["n"] and "+" not in key["n"] and "/" not in key["n"]
    # The private half never leaves the service.
    assert "d" not in key and "p" not in key and "q" not in key


async def test_jwks_verifies_a_freshly_issued_access_token(client: httpx.AsyncClient) -> None:
    """The whole point of the endpoint: another service can verify our tokens.

    This is the same path the Go services take — fetch the JWKS, pick the key
    by `kid`, verify RS256 — with PyJWK standing in for their JWKS client.
    """
    registered = await register(client)
    access_token = registered.json()["access_token"]
    user = registered.json()["user"]

    jwks = (await client.get("/.well-known/jwks.json")).json()

    header = jwt.get_unverified_header(access_token)
    assert header["alg"] == "RS256"
    assert header["kid"] == jwks["keys"][0]["kid"]

    public_key = jwt.PyJWK.from_dict(jwks["keys"][0]).key

    claims = jwt.decode(access_token, public_key, algorithms=["RS256"])

    # Exactly the claims CLAUDE.md specifies, and nothing else.
    assert set(claims) == {"sub", "email", "iat", "exp", "jti", "typ"}
    assert claims["sub"] == user["id"]
    assert claims["email"] == user["email"]
    assert claims["typ"] == "access"
    assert claims["exp"] - claims["iat"] == 900

    issued_at = dt.datetime.fromtimestamp(claims["iat"], dt.UTC)
    assert abs((dt.datetime.now(dt.UTC) - issued_at).total_seconds()) < 60


async def test_a_token_signed_by_another_key_does_not_verify(client: httpx.AsyncClient) -> None:
    from cryptography.hazmat.primitives.asymmetric import rsa

    jwks = (await client.get("/.well-known/jwks.json")).json()
    public_key = jwt.PyJWK.from_dict(jwks["keys"][0]).key

    impostor = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    forged = jwt.encode({"sub": "someone", "typ": "access"}, impostor, algorithm="RS256")

    with pytest.raises(jwt.InvalidSignatureError):
        jwt.decode(forged, public_key, algorithms=["RS256"])


async def test_refresh_tokens_are_opaque_not_jwts(client: httpx.AsyncClient) -> None:
    refresh_token = (await register(client)).json()["refresh_token"]

    assert "." not in refresh_token
    # secrets.token_urlsafe(32) -> 43 base64url characters.
    assert len(refresh_token) == 43
