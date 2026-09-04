"""Access-token signing, the JWKS document, and refresh-token secrets.

The RS256 private key exists only in this service. Every other service reads
the public half from `GET /.well-known/jwks.json` and verifies tokens itself
— no service trusts a gateway-injected identity header.
"""

from __future__ import annotations

import base64
import datetime as dt
import hashlib
import secrets
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import jwt
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.rsa import RSAPrivateKey

from app.errors import InvalidAccessToken

ALGORITHM = "RS256"
TOKEN_TYPE_ACCESS = "access"
# 32 bytes of CSPRNG output, per CLAUDE.md. token_urlsafe takes a byte count
# and returns base64url of it, so this is 32 bytes of entropy, not 32 chars.
REFRESH_TOKEN_BYTES = 32


class SigningKeyError(RuntimeError):
    """The configured private key is missing, unreadable, or not RSA."""


def _b64u_uint(value: int) -> str:
    length = (value.bit_length() + 7) // 8 or 1
    return base64.urlsafe_b64encode(value.to_bytes(length, "big")).rstrip(b"=").decode("ascii")


@dataclass(frozen=True)
class AccessToken:
    """A minted access token and the two things a caller needs about it.

    `jti` is returned rather than left inside the encoded token because the
    refresh token row stores it: it is what lets a password change spare the
    session that asked for it. See `RefreshToken.access_token_jti`.
    """

    token: str
    jti: uuid.UUID
    expires_in: int


@dataclass(frozen=True)
class AccessTokenClaims:
    """The verified claims of an access token."""

    user_id: uuid.UUID
    email: str
    jti: uuid.UUID


@dataclass(frozen=True)
class TokenIssuer:
    """Holds the loaded key and mints access tokens from it."""

    private_key: RSAPrivateKey
    key_id: str
    access_token_ttl: int

    @property
    def jwks(self) -> dict[str, Any]:
        """The public key as a JWKS document.

        Derived from the private key rather than read from a separate public
        PEM: the two files cannot drift if only one of them is authoritative.
        """
        numbers = self.private_key.public_key().public_numbers()
        return {
            "keys": [
                {
                    "kty": "RSA",
                    "use": "sig",
                    "alg": ALGORITHM,
                    "kid": self.key_id,
                    "n": _b64u_uint(numbers.n),
                    "e": _b64u_uint(numbers.e),
                }
            ]
        }

    def issue_access_token(self, *, user_id: uuid.UUID, email: str) -> AccessToken:
        """Mint a signed access token.

        Claims are exactly the five in CLAUDE.md plus `typ`. Anything else a
        verifier might want — roles, profile fields — is a lookup, not a
        claim; tokens live 15 minutes and stale claims outlive the fact.
        """
        now = dt.datetime.now(dt.UTC)
        expires_at = now + dt.timedelta(seconds=self.access_token_ttl)
        jti = uuid.uuid4()
        claims = {
            "sub": str(user_id),
            "email": email,
            "iat": int(now.timestamp()),
            "exp": int(expires_at.timestamp()),
            "jti": str(jti),
            "typ": TOKEN_TYPE_ACCESS,
        }
        token = jwt.encode(
            claims,
            self.private_key,  # type: ignore[arg-type]
            algorithm=ALGORITHM,
            headers={"kid": self.key_id},
        )
        return AccessToken(token=token, jti=jti, expires_in=self.access_token_ttl)

    def verify_access_token(self, token: str) -> AccessTokenClaims:
        """Verify a bearer token and return its claims, or raise.

        Identity verifies against its own key rather than fetching its own
        JWKS over HTTP — it *is* the issuer. Every other service does the
        JWKS dance instead, and none of them trusts a gateway-injected
        header (CLAUDE.md rule 6).

        `algorithms` is pinned to RS256 and never read from the token's own
        header: accepting the header's word for it is how a service ends up
        verifying an `alg: none` or an HS256 token signed with its own public
        key. Every failure raises the same error — see `InvalidAccessToken`.
        """
        try:
            claims = jwt.decode(
                token,
                self.private_key.public_key(),  # type: ignore[arg-type]
                algorithms=[ALGORITHM],
                options={"require": ["sub", "exp", "iat", "jti"]},
            )
        except jwt.PyJWTError as exc:
            raise InvalidAccessToken() from exc

        # A refresh token is opaque and can never be presented here, but a
        # future token type could be, and one minted for a different purpose
        # must not authenticate a request.
        if claims.get("typ") != TOKEN_TYPE_ACCESS:
            raise InvalidAccessToken()

        try:
            return AccessTokenClaims(
                user_id=uuid.UUID(claims["sub"]),
                email=str(claims.get("email", "")),
                jti=uuid.UUID(claims["jti"]),
            )
        except (KeyError, ValueError, TypeError) as exc:
            raise InvalidAccessToken() from exc


def load_issuer(path: Path, *, key_id: str, access_token_ttl: int) -> TokenIssuer:
    """Read the private key at startup, or refuse to start.

    A service that boots without a usable signing key would accept
    registrations and fail every login, which is worse than not booting.
    """
    try:
        pem = path.read_bytes()
    except OSError as exc:
        raise SigningKeyError(f"cannot read JWT private key at {path}: {exc}") from exc

    try:
        key = serialization.load_pem_private_key(pem, password=None)
    except Exception as exc:
        raise SigningKeyError(f"JWT private key at {path} is not a readable PEM: {exc}") from exc

    if not isinstance(key, RSAPrivateKey):
        raise SigningKeyError(
            f"JWT private key at {path} is {type(key).__name__}, but RS256 requires an RSA key"
        )
    if key.key_size < 2048:
        raise SigningKeyError(
            f"JWT private key at {path} is {key.key_size} bits; RS256 requires at least 2048"
        )

    return TokenIssuer(private_key=key, key_id=key_id, access_token_ttl=access_token_ttl)


def generate_refresh_token() -> tuple[str, bytes]:
    """Return the opaque token to hand out and the digest to store."""
    token = secrets.token_urlsafe(REFRESH_TOKEN_BYTES)
    return token, hash_refresh_token(token)


def hash_refresh_token(token: str) -> bytes:
    """SHA-256 of the token, stored raw in a bytea column.

    No salt and no KDF on purpose: the input is already 32 bytes of uniform
    randomness, so there is no dictionary to attack and a slow hash would
    only tax every refresh.
    """
    return hashlib.sha256(token.encode("utf-8")).digest()


def generate_verification_token() -> str:
    """Single-use token carried by `user.registered`. Never logged."""
    return secrets.token_urlsafe(32)
