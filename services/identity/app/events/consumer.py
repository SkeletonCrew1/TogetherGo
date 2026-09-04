"""The `identity.trip-events` consumer: an asyncio task, not a deployable.

Reads `trip.completed` off the queue the broker declared for this service and
hands it to a handler that opens rating windows. The shape mirrors the Go
services' consumers (`services/chat/internal/events/consumer.go`) because the
contract they answer to is the same one — CLAUDE.md rule 7 says duplicate the
30 lines rather than share a package across two languages.

Three properties are the contract rather than implementation detail:

* **The consumer never declares topology.** Exchanges, queues and bindings
  are imported by the broker at boot from `deploy/rabbitmq/definitions.json`.
  A consumer that declared its own queue would eventually declare it with
  different arguments and earn a PRECONDITION_FAILED.
* **Manual ack, always.** An auto-ack consumer acknowledges on delivery and
  loses every in-flight message on a crash — the exact failure the publishing
  side's outbox went to such lengths to rule out.
* **`requeue=True` is never used.** It puts the message back at the head of
  the same queue, it is redelivered at once, it fails again, and the consumer
  spins at full speed on a poison message. Retrying happens here, where the
  backoff is ours, and what survives it goes to the DLQ.
"""

from __future__ import annotations

import asyncio
import json
import logging
import random
from typing import Protocol

import aio_pika
from aio_pika.abc import AbstractIncomingMessage, AbstractQueue, AbstractRobustConnection

from app.events.payloads import Envelope, MalformedPayload

logger = logging.getLogger(__name__)

# The queue this service is bound to. Named in deploy/rabbitmq/definitions.json
# and in contracts/events.md; repeated here because the consumer has to ask for
# it by name, not because it is free to choose.
QUEUE = "identity.trip-events"

# The 10 contracts/events.md fixes for every consumer in the system. Unacked
# messages beyond it stay on the broker, so a restart redelivers them and a
# slow consumer accumulates no unbounded backlog.
DEFAULT_PREFETCH = 10

# One try plus the three retries the contract prescribes for a transient
# failure; after the fourth the message is dead-lettered.
HANDLER_ATTEMPTS = 4

# The delay before the second attempt. The third and fourth double it, giving
# the contract's 200/400/800 ms ladder.
DEFAULT_RETRY_BASE = 0.2

# ±20 %, so replicas that failed on the same database outage do not come back
# in lockstep and knock it over again. `random` and not `secrets`: this is
# scheduling noise, and the only property it needs is that two processes
# disagree.
RETRY_JITTER = 0.2


class EventHandler(Protocol):
    """What the consumer needs from the domain, and nothing more.

    Returns whether the event was applied — `False` means it had already been
    processed, which is not an error and is acknowledged like any success.
    The handler owns its own transaction, and the idempotency check and the
    work happen inside it (CLAUDE.md rule 5).
    """

    async def handle(self, envelope: Envelope) -> bool: ...


class TripEventsConsumer:
    def __init__(
        self,
        *,
        handler: EventHandler,
        amqp_url: str,
        queue: str = QUEUE,
        prefetch: int = DEFAULT_PREFETCH,
        retry_base: float = DEFAULT_RETRY_BASE,
        base_backoff: float = 1.0,
        max_backoff: float = 30.0,
    ) -> None:
        self._handler = handler
        self._amqp_url = amqp_url
        self._queue_name = queue
        self._prefetch = prefetch
        self._retry_base = retry_base
        self._base_backoff = base_backoff
        self._max_backoff = max_backoff

        self._connection: AbstractRobustConnection | None = None
        self._queue: AbstractQueue | None = None
        self._task: asyncio.Task[None] | None = None
        self._stopping = asyncio.Event()

    # -- lifecycle ----------------------------------------------------------

    async def start(self) -> None:
        self._stopping.clear()
        self._task = asyncio.create_task(self._run(), name="trip-events-consumer")

    async def stop(self) -> None:
        self._stopping.set()
        if self._task is not None:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
            self._task = None
        await self._close_connection()
        logger.info("trip events consumer stopped")

    @property
    def connected(self) -> bool:
        return self._connection is not None and not self._connection.is_closed

    # -- the loop -----------------------------------------------------------

    async def _run(self) -> None:
        """Connect, consume, back off, connect again.

        A broker restart costs one backoff and no supervision: there is no
        health check that restarts this process and no separate state machine
        to get wrong. Deliveries left unacked when the connection drops are
        redelivered, and `processed_events` makes that harmless.
        """
        delay = 0.0
        while not self._stopping.is_set():
            if delay:
                await self._sleep(delay)
                if self._stopping.is_set():
                    return
            try:
                await self._consume()
                delay = self._base_backoff
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                delay = min(max(delay * 2, self._base_backoff), self._max_backoff)
                logger.warning(
                    "trip events consumer disconnected",
                    extra={
                        "error_type": type(exc).__name__,
                        "error": str(exc),
                        "retry_in": delay,
                    },
                )

    async def _consume(self) -> None:
        """Hold a consumer open until shutdown or the connection is gone.

        A broker that drops mid-session is aio_pika's problem, not this
        loop's: `connect_robust` reconnects and restores the channel, the QoS
        and the consumer on its own. What the loop above handles is the case
        this cannot — a broker that was never reachable in the first place, so
        there is no connection to be robust about yet.
        """
        # Anything left from a previous attempt goes first. Calling
        # connect_robust again on top of a live connection would leak it, and
        # with it a second consumer on the same queue competing for messages.
        await self._close_connection()

        self._connection = await aio_pika.connect_robust(self._amqp_url)
        channel = await self._connection.channel()
        await channel.set_qos(prefetch_count=self._prefetch)
        # ensure=False skips the passive declare: the topology is the broker's
        # and we only need a handle to consume from.
        self._queue = await channel.get_queue(self._queue_name, ensure=False)

        await self._queue.consume(self._on_message, no_ack=False)
        logger.info("trip events consumer started", extra={"queue": self._queue_name})

        # aio_pika delivers on its own task, so this one has nothing to do but
        # wait for a reason to stop. `closed()` resolves when the connection is
        # finally closed — which for a robust connection means someone closed
        # it, not that the socket blinked.
        closed = asyncio.ensure_future(self._connection.closed())
        stopping = asyncio.ensure_future(self._stopping.wait())
        try:
            await asyncio.wait({closed, stopping}, return_when=asyncio.FIRST_COMPLETED)
        finally:
            for pending in (closed, stopping):
                if not pending.done():
                    pending.cancel()

        # Returning normally on shutdown and raising otherwise is what drives
        # the backoff above.
        if not self._stopping.is_set():
            raise ConnectionError("broker connection closed")

    async def _close_connection(self) -> None:
        if self._connection is not None and not self._connection.is_closed:
            await self._connection.close()
        self._connection = None
        self._queue = None

    # -- one message --------------------------------------------------------

    async def _on_message(self, message: AbstractIncomingMessage) -> None:
        """Decide one message's fate: ack, or dead-letter. There is no third.

        Ack covers "applied", "duplicate" and "an event type this build does
        not know" — the contract is explicit about the last one: log at warn,
        mark it processed, ack. An unknown event is a message meant for a
        newer version of this service, and dead-lettering it would fill a DLQ
        with things nobody will ever act on.

        Dead-letter covers "will never be processable" — a body that is not
        JSON, an envelope with no event_id — and "good, but four attempts
        against an unavailable database were not enough". The DLQ is a human
        queue: nothing drains it on a timer, and a message in it is an alert.
        Replaying from it is safe, because `processed_events` makes a replay a
        no-op for anything already handled.
        """
        try:
            envelope = Envelope(json.loads(message.body))
        except (json.JSONDecodeError, MalformedPayload, UnicodeDecodeError) as exc:
            await self._dead_letter(message, "body is not a valid event envelope", exc)
            return

        context = {"event_id": str(envelope.event_id), "event_type": envelope.event_type}

        applied, error = await self._apply_with_retries(envelope, context)
        if error is not None:
            await self._dead_letter(message, "handler failed", error, context)
            return

        try:
            await message.ack()
        except Exception as exc:
            # The work is committed and processed_events holds the id, so the
            # redelivery a failed ack causes is absorbed. Nothing to undo.
            logger.error(
                "ack failed; the event is applied and a redelivery will be ignored",
                extra={**context, "error": str(exc)},
            )
            return

        if not applied:
            logger.debug("trip event already processed, ignored", extra=context)

    async def _apply_with_retries(
        self, envelope: Envelope, context: dict[str, str]
    ) -> tuple[bool, Exception | None]:
        """Run the handler up to `HANDLER_ATTEMPTS` times.

        A malformed payload returns immediately: retrying a body that is not
        the event it claims to be three more times is three more log lines and
        the same answer.
        """
        delay = self._retry_base
        last: Exception | None = None

        for attempt in range(1, HANDLER_ATTEMPTS + 1):
            try:
                return await self._handler.handle(envelope), None
            except MalformedPayload as exc:
                return False, exc
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                last = exc
                if attempt == HANDLER_ATTEMPTS:
                    break
                logger.warning(
                    "trip event failed, retrying",
                    extra={
                        **context,
                        "attempt": attempt,
                        "retry_in": delay,
                        "error": str(exc),
                    },
                )
                await asyncio.sleep(_jittered(delay))
                delay *= 2

        return False, last

    async def _dead_letter(
        self,
        message: AbstractIncomingMessage,
        reason: str,
        cause: Exception | None = None,
        context: dict[str, str] | None = None,
    ) -> None:
        logger.error(
            "trip event dead-lettered",
            extra={
                **(context or {}),
                "reason": reason,
                "error_type": type(cause).__name__ if cause else None,
                "error": str(cause) if cause else None,
            },
        )
        # requeue=False, so the broker routes it to identity.trip-events.dlq
        # via the queue's dead-letter exchange rather than back to the head of
        # this queue.
        await message.nack(requeue=False)

    async def _sleep(self, seconds: float) -> None:
        """Sleep, but wake immediately on shutdown."""
        try:
            await asyncio.wait_for(self._stopping.wait(), timeout=seconds)
        except TimeoutError:
            pass


def _jittered(delay: float) -> float:
    spread = delay * RETRY_JITTER
    return delay - spread + random.random() * 2 * spread
