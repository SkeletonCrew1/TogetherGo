"""Test fixtures.

Everything runs against a real Postgres and a real Redis in containers. The
schema comes from `alembic upgrade head`, not from `metadata.create_all`, so
the migration is exercised by every test run — a migration that drifts from
the models is a bug the suite should catch, not one production finds.

The outbox relay is disabled: publishing needs a broker, and what these tests
care about is that the row lands in the table inside the right transaction.
"""

from __future__ import annotations

import os
import subprocess
import sys
import uuid
from collections.abc import AsyncIterator, Iterator
from dataclasses import dataclass
from pathlib import Path

import httpx
import pytest
import pytest_asyncio
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from fastapi import FastAPI
from redis.asyncio import Redis
from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker
from testcontainers.core.container import DockerContainer
from testcontainers.core.wait_strategies import LogMessageWaitStrategy
from testcontainers.community.postgres import PostgresContainer
from testcontainers.community.redis import RedisContainer

from app.config import Settings
from app.main import create_app

SERVICE_ROOT = Path(__file__).resolve().parents[1]
REPO_ROOT = SERVICE_ROOT.parents[1]

# Long enough to satisfy the config's min_length, and obviously not a secret.
INTERNAL_TOKEN = "test-internal-token-0123456789"


# --- containers -------------------------------------------------------------


@pytest.fixture(scope="session")
def postgres_url() -> Iterator[str]:
    # The same image the platform runs, so citext behaves identically here.
    with PostgresContainer("postgis/postgis:16-3.4", driver="asyncpg") as container:
        yield container.get_connection_url()


@pytest.fixture(scope="session")
def redis_url() -> Iterator[str]:
    with RedisContainer("redis:7-alpine") as container:
        host = container.get_container_host_ip()
        port = container.get_exposed_port(6379)
        yield f"redis://{host}:{port}/0"


@pytest.fixture(scope="session")
def migrated_database(postgres_url: str) -> str:
    """Run the real migration once for the whole session."""
    env = {**os.environ, "IDENTITY_DATABASE_URL": postgres_url}
    result = subprocess.run(
        [sys.executable, "-m", "alembic", "upgrade", "head"],
        cwd=SERVICE_ROOT,
        env=env,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"alembic upgrade head failed:\n{result.stdout}\n{result.stderr}")
    return postgres_url


@pytest.fixture(scope="session")
def broker_url() -> Iterator[str]:
    """A throwaway RabbitMQ carrying the repository's real topology.

    Booted with `deploy/rabbitmq/{rabbitmq.conf,definitions.json}`, so the
    exchange, `identity.trip-events` and its DLQ are the ones Compose runs
    rather than a topology the test invented to agree with itself. Nothing
    here declares anything, which is the same rule the service follows.

    Only the consumer test needs it, so it is a fixture rather than something
    every run pays for.
    """
    container = (
        DockerContainer("rabbitmq:3.13-management")
        .with_exposed_ports(5672)
        .with_env("RABBITMQ_DEFAULT_USER", "togethergo")
        .with_env("RABBITMQ_DEFAULT_PASS", "togethergo")
        .with_volume_mapping(
            str(REPO_ROOT / "deploy" / "rabbitmq" / "rabbitmq.conf"),
            "/etc/rabbitmq/rabbitmq.conf",
            "ro",
        )
        .with_volume_mapping(
            str(REPO_ROOT / "deploy" / "rabbitmq" / "definitions.json"),
            "/etc/rabbitmq/definitions.json",
            "ro",
        )
    )
    # An open port is not a loaded topology. Waiting for the startup banner is
    # what keeps a test from publishing into a half-declared broker and then
    # wondering where its message went.
    container.waiting_for(LogMessageWaitStrategy("Server startup complete"))
    with container:
        host = container.get_container_host_ip()
        port = container.get_exposed_port(5672)
        yield f"amqp://togethergo:togethergo@{host}:{port}/"


# --- signing key ------------------------------------------------------------


@pytest.fixture(scope="session")
def private_key_path(tmp_path_factory: pytest.TempPathFactory) -> Path:
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    path = tmp_path_factory.mktemp("keys") / "jwt_private.pem"
    path.write_bytes(
        key.private_bytes(
            encoding=serialization.Encoding.PEM,
            format=serialization.PrivateFormat.PKCS8,
            encryption_algorithm=serialization.NoEncryption(),
        )
    )
    return path


# --- application ------------------------------------------------------------


@pytest.fixture
def settings(migrated_database: str, redis_url: str, private_key_path: Path) -> Settings:
    return Settings(
        IDENTITY_DATABASE_URL=migrated_database,
        REDIS_URL=redis_url,
        RABBITMQ_URL="amqp://guest:guest@localhost:5672/",
        JWT_PRIVATE_KEY_PATH=private_key_path,
        JWT_KEY_ID="test-key-1",
        INTERNAL_API_TOKEN=INTERNAL_TOKEN,
        OUTBOX_RELAY_ENABLED=False,
        # Both need a broker, and what these tests care about is the handler
        # and the rows it writes. `tests/test_ratings.py` drives the handler
        # directly, which is the same code path a delivery takes.
        TRIP_EVENTS_CONSUMER_ENABLED=False,
        # Off so a sweep never races a test that has just written an expired
        # window; tests/test_ratings.py forces a pass explicitly instead.
        RATING_SWEEP_ENABLED=False,
        LOG_LEVEL="warning",
        ENVIRONMENT="test",
    )


@pytest_asyncio.fixture
async def app(settings: Settings) -> AsyncIterator[FastAPI]:
    application = create_app(settings)
    async with application.router.lifespan_context(application):
        await _reset_state(application)
        yield application


async def _reset_state(application: FastAPI) -> None:
    """Empty every table and the rate-limit budget between tests."""
    async with application.state.engine.begin() as connection:
        await connection.execute(
            text(
                "TRUNCATE ratings, pending_ratings, user_rating, processed_events, "
                "refresh_tokens, outbox, users RESTART IDENTITY CASCADE"
            )
        )
    redis: Redis = application.state.redis
    await redis.flushdb()


@pytest_asyncio.fixture
async def client(app: FastAPI) -> AsyncIterator[httpx.AsyncClient]:
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(
        transport=transport,
        base_url="http://identity",
        headers={"user-agent": "pytest/1.0"},
    ) as http_client:
        yield http_client


@pytest_asyncio.fixture
async def session(app: FastAPI) -> AsyncIterator[AsyncSession]:
    """A session for reaching into the database and asserting on rows."""
    factory: async_sessionmaker[AsyncSession] = app.state.sessionmaker
    async with factory() as db_session:
        yield db_session


# --- helpers ----------------------------------------------------------------


def registration_payload(**overrides) -> dict:
    """A valid registration body; override one field to make it invalid."""
    payload = {
        "email": f"traveller-{uuid.uuid4().hex[:12]}@example.com",
        "password": "correct horse battery",
        "full_name": "Alex Traveller",
        "birth_date": "1994-05-17",
        "bio": "Weekend hiker, slow walker, good with maps.",
    }
    payload.update(overrides)
    return payload


async def register(client: httpx.AsyncClient, **overrides) -> httpx.Response:
    return await client.post("/api/auth/register", json=registration_payload(**overrides))


@dataclass(frozen=True)
class Account:
    """A registered user and one signed-in session belonging to it."""

    id: str
    email: str
    password: str
    access_token: str
    refresh_token: str

    @property
    def auth(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self.access_token}"}


async def register_account(client: httpx.AsyncClient, **overrides) -> Account:
    """Register, and hand back everything a test needs to act as that user."""
    payload = registration_payload(**overrides)
    response = await client.post("/api/auth/register", json=payload)
    assert response.status_code == 201, response.text
    body = response.json()
    return Account(
        id=body["user"]["id"],
        email=payload["email"],
        password=payload["password"],
        access_token=body["access_token"],
        refresh_token=body["refresh_token"],
    )


async def login(client: httpx.AsyncClient, account: Account) -> Account:
    """Open a second session for an existing account."""
    response = await client.post(
        "/api/auth/login",
        json={"email": account.email, "password": account.password},
    )
    assert response.status_code == 200, response.text
    body = response.json()
    return Account(
        id=account.id,
        email=account.email,
        password=account.password,
        access_token=body["access_token"],
        refresh_token=body["refresh_token"],
    )
