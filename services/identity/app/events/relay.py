"""The outbox relay: an asyncio task inside this service, not a deployable.

Polls the outbox every second in batches of 100, publishes to
`togethergo.events` with publisher confirms on, and marks the confirmed rows
published — all in one transaction. Dying anywhere before the commit rolls
back the marks, releases the locks and republishes the batch on the next
tick, which is exactly where at-least-once delivery comes from.

The relay never declares topology. Exchanges, queues and bindings are
imported by the broker at boot from deploy/rabbitmq/definitions.json; a
service that declared its own would eventually declare it with different
arguments and earn a PRECONDITION_FAILED.
"""

from __future__ import annotations

import asyncio
import datetime as dt
import json
import logging

import aio_pika
from aio_pika.abc import AbstractExchange, AbstractRobustConnection
from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from app.events.envelope import build_envelope
from app.repositories.outbox import OutboxRepository

logger = logging.getLogger(__name__)

CLEANUP_INTERVAL_SECONDS = 3600
CLEANUP_CHUNK = 10_000


class OutboxRelay:
    def __init__(
        self,
        *,
        sessionmaker: async_sessionmaker[AsyncSession],
        amqp_url: str,
        exchange_name: str,
        poll_interval: float = 1.0,
        batch_size: int = 100,
        retention_days: int = 7,
    ) -> None:
        self._sessionmaker = sessionmaker
        self._amqp_url = amqp_url
        self._exchange_name = exchange_name
        self._poll_interval = poll_interval
        self._batch_size = batch_size
        self._retention_days = retention_days

        self._connection: AbstractRobustConnection | None = None
        self._exchange: AbstractExchange | None = None
        self._tasks: list[asyncio.Task[None]] = []
        self._stopping = asyncio.Event()

    # -- lifecycle ----------------------------------------------------------

    async def start(self) -> None:
        self._stopping.clear()
        # Connect eagerly so /readyz can report on the broker from the first
        # probe rather than only after the first event is published. A broker
        # that is not up yet is not fatal: the publish loop keeps retrying.
        try:
            await self._ensure_exchange()
        except Exception as exc:
            logger.warning(
                "outbox relay could not reach the broker at startup",
                extra={"error_type": type(exc).__name__, "error": str(exc)},
            )

        self._tasks = [
            asyncio.create_task(self._publish_loop(), name="outbox-relay"),
            asyncio.create_task(self._cleanup_loop(), name="outbox-cleanup"),
        ]
        logger.info("outbox relay started", extra={"exchange": self._exchange_name})

    async def stop(self) -> None:
        self._stopping.set()
        for task in self._tasks:
            task.cancel()
        for task in self._tasks:
            try:
                await task
            except asyncio.CancelledError:
                pass
        self._tasks = []
        if self._connection is not None and not self._connection.is_closed:
            await self._connection.close()
        self._connection = None
        self._exchange = None
        logger.info("outbox relay stopped")

    @property
    def connected(self) -> bool:
        return self._connection is not None and not self._connection.is_closed

    # -- broker -------------------------------------------------------------

    async def _ensure_exchange(self) -> AbstractExchange:
        if self._exchange is not None and self.connected:
            return self._exchange

        self._connection = await aio_pika.connect_robust(self._amqp_url)
        channel = await self._connection.channel(publisher_confirms=True)
        # ensure=False skips the passive declare entirely: the topology is the
        # broker's, and we only need a handle to publish through.
        self._exchange = await channel.get_exchange(self._exchange_name, ensure=False)
        return self._exchange

    # -- loops --------------------------------------------------------------

    async def _publish_loop(self) -> None:
        while not self._stopping.is_set():
            try:
                published = await self._drain_once()
                # A full batch means there is probably more waiting; go
                # straight round again instead of idling for a second.
                if published >= self._batch_size:
                    continue
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                # Broker down, database down, anything: the rows are still
                # unpublished, so the next tick retries them. Nothing is lost
                # by swallowing this beyond a second of latency.
                logger.warning(
                    "outbox relay tick failed",
                    extra={"error_type": type(exc).__name__, "error": str(exc)},
                )
            await self._sleep(self._poll_interval)

    async def _drain_once(self) -> int:
        async with self._sessionmaker() as session:
            async with session.begin():
                repo = OutboxRepository(session)
                rows = await repo.claim_unpublished(self._batch_size)
                if not rows:
                    return 0

                exchange = await self._ensure_exchange()
                confirmed: list = []

                for row in rows:
                    envelope = build_envelope(
                        event_id=row.id,
                        event_type=row.event_type,
                        aggregate_id=row.aggregate_id,
                        occurred_at=row.created_at,
                        payload=row.payload,
                    )
                    message = aio_pika.Message(
                        body=json.dumps(envelope, separators=(",", ":")).encode("utf-8"),
                        content_type="application/json",
                        content_encoding="utf-8",
                        delivery_mode=aio_pika.DeliveryMode.PERSISTENT,
                        message_id=str(row.id),
                        type=row.event_type,
                        timestamp=row.created_at,
                    )
                    # publisher_confirms=True makes this await the broker's
                    # ack. A row whose publish is nacked or times out stays
                    # unmarked and is retried on the next tick.
                    await exchange.publish(message, routing_key=row.event_type)
                    confirmed.append(row.id)

                await repo.mark_published(confirmed, now=dt.datetime.now(dt.UTC))

        logger.info(
            "outbox batch published",
            extra={"count": len(confirmed), "exchange": self._exchange_name},
        )
        return len(confirmed)

    async def _cleanup_loop(self) -> None:
        while not self._stopping.is_set():
            await self._sleep(CLEANUP_INTERVAL_SECONDS)
            if self._stopping.is_set():
                return
            try:
                deleted = await self._cleanup_once()
                if deleted:
                    logger.info("outbox rows pruned", extra={"count": deleted})
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                logger.warning(
                    "outbox cleanup failed",
                    extra={"error_type": type(exc).__name__, "error": str(exc)},
                )

    async def _cleanup_once(self) -> int:
        cutoff = dt.datetime.now(dt.UTC) - dt.timedelta(days=self._retention_days)
        total = 0
        while True:
            async with self._sessionmaker() as session:
                async with session.begin():
                    deleted = await OutboxRepository(session).delete_published_before(
                        cutoff, limit=CLEANUP_CHUNK
                    )
            total += deleted
            if deleted < CLEANUP_CHUNK:
                return total

    async def _sleep(self, seconds: float) -> None:
        """Sleep, but wake immediately on shutdown."""
        try:
            await asyncio.wait_for(self._stopping.wait(), timeout=seconds)
        except TimeoutError:
            pass
