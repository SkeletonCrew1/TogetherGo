"""Argon2id password hashing.

One module-level hasher so the cost parameters are stated once. The dummy
hash exists so that "no such user" costs the same as "wrong password" —
without it, response time is an oracle for which addresses are registered.
"""

from __future__ import annotations

import secrets

from argon2 import PasswordHasher
from argon2.exceptions import InvalidHashError, VerificationError, VerifyMismatchError
from argon2.low_level import Type

# RFC 9106's second recommended profile: 64 MiB, 3 passes, 4 lanes.
_hasher = PasswordHasher(
    time_cost=3,
    memory_cost=65536,
    parallelism=4,
    hash_len=32,
    salt_len=16,
    type=Type.ID,
)

# Hashed at import from a random secret, so it is a real Argon2id digest with
# the current parameters and verifying against it takes exactly as long as
# verifying against a real user's. Nothing can ever match it.
_DUMMY_HASH = _hasher.hash(secrets.token_urlsafe(32))


def hash_password(password: str) -> str:
    return _hasher.hash(password)


def verify_password(password_hash: str, password: str) -> bool:
    try:
        return _hasher.verify(password_hash, password)
    except (VerifyMismatchError, VerificationError, InvalidHashError):
        return False


def verify_dummy(password: str) -> None:
    """Burn one verification's worth of time when the user does not exist."""
    verify_password(_DUMMY_HASH, password)


def needs_rehash(password_hash: str) -> bool:
    """True when the stored digest predates the current cost parameters."""
    try:
        return _hasher.check_needs_rehash(password_hash)
    except InvalidHashError:
        return False
