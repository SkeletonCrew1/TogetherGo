"""Engine and session lifecycle.

One `AsyncEngine` per process, created at startup and disposed at shutdown.
Sessions are per-request (or per-relay-tick) and are always used as an
explicit transaction boundary — `async with session.begin()` — so the
"domain write and outbox insert in the same transaction" rule is enforced by
the shape of the code rather than by remembering to call commit.
"""

from __future__ import annotations

from collections.abc import AsyncIterator

from sqlalchemy.ext.asyncio import (
    AsyncEngine,
    AsyncSession,
    async_sessionmaker,
    create_async_engine,
)


def create_engine(database_url: str, **kwargs) -> AsyncEngine:
    return create_async_engine(
        database_url,
        pool_size=10,
        max_overflow=5,
        pool_pre_ping=True,
        # Sessions are short-lived and every statement runs inside an explicit
        # transaction; SQLAlchemy's own statement cache is enough.
        echo=False,
        **kwargs,
    )


def create_sessionmaker(engine: AsyncEngine) -> async_sessionmaker[AsyncSession]:
    return async_sessionmaker(
        engine,
        expire_on_commit=False,
        autoflush=False,
    )


async def session_scope(
    factory: async_sessionmaker[AsyncSession],
) -> AsyncIterator[AsyncSession]:
    async with factory() as session:
        yield session
