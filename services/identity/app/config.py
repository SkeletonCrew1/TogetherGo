"""Configuration, loaded once from the environment and never read again.

Mirrors the Go services' `config.Load()`: a single entry point that either
returns a fully validated settings object or aborts the process. There is no
`os.environ` lookup anywhere else in the service.
"""

from __future__ import annotations

import functools
from pathlib import Path

from pydantic import Field, field_validator
from pydantic_settings import BaseSettings, SettingsConfigDict


class ConfigError(RuntimeError):
    """Raised when the environment cannot produce a usable configuration."""


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        # Nothing is read from a .env file: in Compose the environment is the
        # source of truth, and locally `make` exports it. A stray .env inside
        # the service directory shadowing the real config is a debugging trap.
        env_file=None,
        extra="ignore",
        populate_by_name=True,
    )

    # --- Datastores ---------------------------------------------------------
    database_url: str = Field(alias="IDENTITY_DATABASE_URL")
    redis_url: str = Field(alias="REDIS_URL")

    # --- Broker -------------------------------------------------------------
    rabbitmq_url: str = Field(alias="RABBITMQ_URL")
    rabbitmq_exchange: str = Field(default="togethergo.events", alias="RABBITMQ_EXCHANGE")

    # --- Token signing ------------------------------------------------------
    jwt_private_key_path: Path = Field(alias="JWT_PRIVATE_KEY_PATH")
    jwt_algorithm: str = Field(default="RS256", alias="JWT_ALGORITHM")
    jwt_key_id: str = Field(alias="JWT_KEY_ID")
    access_token_ttl: int = Field(default=900, alias="ACCESS_TOKEN_TTL", gt=0)
    refresh_token_ttl: int = Field(default=2_592_000, alias="REFRESH_TOKEN_TTL", gt=0)

    # --- Service-to-service --------------------------------------------------
    # Shared secret every /internal caller must present as X-Internal-Token.
    # Required, with no default: a service that boots with an empty internal
    # token would either reject every internal call or — worse, if the check
    # were written carelessly — accept every one of them.
    internal_api_token: str = Field(alias="INTERNAL_API_TOKEN", min_length=16)

    # --- Rate limiting ------------------------------------------------------
    # 10 attempts per 15 minutes per IP, sliding window (see CLAUDE.md).
    auth_rate_limit_attempts: int = Field(default=10, alias="AUTH_RATE_LIMIT_ATTEMPTS", gt=0)
    auth_rate_limit_window: int = Field(default=900, alias="AUTH_RATE_LIMIT_WINDOW", gt=0)

    # --- Outbox relay -------------------------------------------------------
    outbox_relay_enabled: bool = Field(default=True, alias="OUTBOX_RELAY_ENABLED")
    outbox_poll_interval: float = Field(default=1.0, alias="OUTBOX_POLL_INTERVAL", gt=0)
    outbox_batch_size: int = Field(default=100, alias="OUTBOX_BATCH_SIZE", gt=0)
    outbox_retention_days: int = Field(default=7, alias="OUTBOX_RETENTION_DAYS", gt=0)

    # --- Trip-events consumer ------------------------------------------------
    # `trip.completed` is what opens a rating window. The queue name is the
    # broker's, declared in deploy/rabbitmq/definitions.json; it is named here
    # because a consumer has to ask for a queue by name, not because this
    # service is free to choose one.
    trip_events_consumer_enabled: bool = Field(
        default=True, alias="TRIP_EVENTS_CONSUMER_ENABLED"
    )
    trip_events_queue: str = Field(
        default="identity.trip-events", alias="TRIP_EVENTS_QUEUE"
    )
    # The 10 contracts/events.md fixes for every consumer in the platform.
    trip_events_prefetch: int = Field(default=10, alias="TRIP_EVENTS_PREFETCH", gt=0)

    # --- Pending-rating sweep -------------------------------------------------
    # Nightly. Housekeeping only: every read already filters on expires_at, so
    # a sweep that never runs changes no answer — it only keeps the table and
    # its partial index the size of the work actually outstanding.
    rating_sweep_enabled: bool = Field(default=True, alias="RATING_SWEEP_ENABLED")
    rating_sweep_interval: float = Field(
        default=86_400.0, alias="RATING_SWEEP_INTERVAL", gt=0
    )

    # --- Runtime ------------------------------------------------------------
    log_level: str = Field(default="info", alias="LOG_LEVEL")
    environment: str = Field(default="local", alias="ENVIRONMENT")

    @field_validator("jwt_algorithm")
    @classmethod
    def _only_rs256(cls, value: str) -> str:
        # The JWKS document and every downstream verifier assume RS256. An
        # environment that asks for something else is a misconfiguration, not
        # a feature to support.
        if value != "RS256":
            raise ValueError("only RS256 is supported")
        return value

    @field_validator("log_level")
    @classmethod
    def _known_level(cls, value: str) -> str:
        level = value.lower()
        if level not in {"debug", "info", "warning", "error", "critical"}:
            raise ValueError(f"unknown log level {value!r}")
        return level


@functools.lru_cache(maxsize=1)
def load() -> Settings:
    """Build the settings object, failing fast with a readable message."""
    try:
        return Settings()  # type: ignore[call-arg]
    except Exception as exc:  # pragma: no cover - exercised by starting badly
        raise ConfigError(f"invalid identity service configuration: {exc}") from exc
