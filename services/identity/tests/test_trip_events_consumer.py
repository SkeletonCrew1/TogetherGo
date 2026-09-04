"""The consumer against a real broker: ack, duplicates, and the DLQ.

Separate from `tests/test_ratings.py`, which drives the handler directly. What
is under test here is the wire — that a published `trip.completed` actually
reaches this service's queue, that a handled message is acknowledged rather
than redelivered forever, and that a message which can never be handled ends
up in `identity.trip-events.dlq` instead of spinning at the head of the queue.

None of that is visible from the handler, and all of it is the kind of thing
that works in review and fails at runtime.
"""

from __future__ import annotations

import asyncio
import datetime as dt
import json
import logging
import uuid

import aio_pika
import httpx
import pytest
import pytest_asyncio
from fastapi import FastAPI
from sqlalchemy import func, select

from app.events.consumer import TripEventsConsumer
from app.models import PendingRating
from app.services.rating_service import RatingEventHandler
from tests.conftest import register_account
from tests.test_ratings import trip_completed

QUEUE = "identity.trip-events"
DLQ = "identity.trip-events.dlq"
EXCHANGE = "togethergo.events"

# Long enough for a message to cross a local broker and commit a transaction,
# short enough that a genuine failure is not a two-minute wait.
SETTLE_TIMEOUT = 15.0


class Broker:
    """The test's own handle on the broker: publish, purge, and count.

    Every reading opens a fresh channel, which is not fastidiousness. aio_pika
    caches the `Queue` object per channel, so a second passive declare of the
    same queue hands back the *first* declaration's counts — poll a depth in a
    loop on one channel and it reports whatever it said the first time,
    forever. A test that waits for a message to arrive would wait fifteen
    seconds and then assert against a number taken before it was sent.
    """

    def __init__(self, connection: aio_pika.abc.AbstractRobustConnection) -> None:
        self._connection = connection

    async def publish(self, envelope: dict) -> None:
        await self.publish_raw(
            json.dumps(envelope).encode(),
            routing_key=envelope["event_type"],
            message_id=envelope["event_id"],
        )

    async def publish_raw(
        self, body: bytes, *, routing_key: str, message_id: str | None = None
    ) -> None:
        async with self._connection.channel() as channel:
            exchange = await channel.get_exchange(EXCHANGE, ensure=False)
            await exchange.publish(
                aio_pika.Message(
                    body=body,
                    content_type="application/json",
                    delivery_mode=aio_pika.DeliveryMode.PERSISTENT,
                    message_id=message_id,
                ),
                routing_key=routing_key,
            )

    async def depth(self, name: str) -> int:
        """How many messages are sitting on a queue right now.

        A passive declare is the only way to ask, and it creates nothing —
        which matters, because a test that declared a queue would be inventing
        the topology it is supposed to be checking.
        """
        async with self._connection.channel() as channel:
            queue = await channel.declare_queue(name, passive=True)
            return queue.declaration_result.message_count

    async def purge(self, name: str) -> None:
        async with self._connection.channel() as channel:
            await (await channel.get_queue(name, ensure=False)).purge()


@pytest_asyncio.fixture
async def broker(broker_url: str) -> Broker:
    """A connection for the test's own publishing and queue inspection."""
    connection = await aio_pika.connect_robust(broker_url)
    handle = Broker(connection)
    # Both queues are drained before each test: the container is shared across
    # the module, and a message left by one test is a failure in the next.
    for name in (QUEUE, DLQ):
        await handle.purge(name)
    try:
        yield handle
    finally:
        await connection.close()


@pytest_asyncio.fixture
async def consumer(app: FastAPI, broker_url: str, broker: Broker):
    """The real consumer, running against the real queue."""
    running = TripEventsConsumer(
        handler=RatingEventHandler(app.state.sessionmaker),
        amqp_url=broker_url,
        queue=QUEUE,
        # Short, so the dead-letter test does not sit through 1.4 s of ladder.
        retry_base=0.01,
    )
    await running.start()
    await _until(lambda: running.connected, "consumer never connected")
    try:
        yield running
    finally:
        await running.stop()


async def _until(predicate, message: str, timeout: float = SETTLE_TIMEOUT) -> None:
    """Poll a condition rather than sleeping a guessed interval."""
    deadline = asyncio.get_running_loop().time() + timeout
    while asyncio.get_running_loop().time() < deadline:
        result = predicate()
        if asyncio.iscoroutine(result):
            result = await result
        if result:
            return
        await asyncio.sleep(0.2)
    raise AssertionError(message)


async def pending_count(app: FastAPI) -> int:
    async with app.state.sessionmaker() as session:
        return (
            await session.execute(select(func.count()).select_from(PendingRating))
        ).scalar_one()


async def depth_is(broker: Broker, name: str, expected: int) -> bool:
    return await broker.depth(name) == expected


class Deliveries:
    """How every message was settled: acknowledged, or dead-lettered.

    Recorded at the AMQP layer rather than inferred from the queue, because
    the queue cannot answer the question. A queue's message count is its
    *ready* messages, and a delivery that was handled but never acknowledged
    is neither ready nor gone — it is unacked, invisible to the count, and
    indistinguishable from one that was acked properly. Counting the calls is
    the only way to tell "acked" from "silently left in flight", and "manual
    ack, always" is the property this file exists to hold.

    `requeued` is here to stay at zero. Nacking with requeue puts a poison
    message back at the head of its own queue, where it is redelivered at
    once, fails again, and pins the consumer at full speed — the one outcome
    contracts/events.md rules out by name.
    """

    def __init__(self) -> None:
        self.acked = 0
        self.dead_lettered = 0
        self.requeued = 0


@pytest.fixture
def deliveries(monkeypatch: pytest.MonkeyPatch) -> Deliveries:
    record = Deliveries()
    ack = aio_pika.message.IncomingMessage.ack
    nack = aio_pika.message.IncomingMessage.nack

    async def counting_ack(self, *args, **kwargs):
        record.acked += 1
        return await ack(self, *args, **kwargs)

    async def counting_nack(self, *args, requeue: bool = False, **kwargs):
        if requeue:
            record.requeued += 1
        else:
            record.dead_lettered += 1
        return await nack(self, *args, requeue=requeue, **kwargs)

    monkeypatch.setattr(aio_pika.message.IncomingMessage, "ack", counting_ack)
    monkeypatch.setattr(aio_pika.message.IncomingMessage, "nack", counting_nack)
    return record


async def test_a_published_trip_completed_opens_the_window_and_is_acked(
    app: FastAPI, client: httpx.AsyncClient, broker: Broker, deliveries, consumer
) -> None:
    """The whole path: exchange, binding, queue, handler, ack."""
    accounts = [await register_account(client) for _ in range(3)]
    await broker.publish(trip_completed(participants=[a.id for a in accounts]))

    await _until(lambda: pending_count(app), "the rating window never opened")
    assert await pending_count(app) == 6

    await _until(lambda: deliveries.acked == 1, "the message was never acknowledged")
    assert (deliveries.dead_lettered, deliveries.requeued) == (0, 0)


async def test_the_consumer_parks_instead_of_churning(broker: Broker, consumer) -> None:
    """The supervision loop must not reconnect while the broker is healthy.

    A regression test with a scar behind it. The loop parks until it has a
    reason to reconnect, and an earlier version parked on an attribute this
    aio_pika does not have — so every pass raised at once, backed off, and
    opened *another* consumer on the same queue. Nothing failed: one of them
    got each message, so every other test in this file still passed, and the
    only symptom was a warning in a log nobody was reading.

    Asserted on that warning rather than on the queue's consumer count. The
    count is a moving target — an abandoned connection is closed by the
    garbage collector whenever it gets round to it, so a reading taken at the
    wrong moment sees exactly the one consumer it expected. A reconnect, on
    the other hand, cannot happen quietly.

    The handler is attached to the consumer's own logger rather than through
    `caplog`, because `app.logging.configure` takes ownership of the root
    handlers when the app is built and pytest's would not survive it.
    """
    churn = _RecordCollector()
    log = logging.getLogger("app.events.consumer")
    log.addHandler(churn)
    try:
        await asyncio.sleep(3)
    finally:
        log.removeHandler(churn)

    assert churn.messages == [], "the consumer reconnected with nothing wrong"
    assert consumer.connected


class _RecordCollector(logging.Handler):
    """Collects warnings and worse from one logger, and nothing else."""

    def __init__(self) -> None:
        super().__init__(level=logging.WARNING)
        self.messages: list[str] = []

    def emit(self, record: logging.LogRecord) -> None:
        self.messages.append(record.getMessage())


async def test_a_redelivered_message_is_absorbed(
    app: FastAPI, client: httpx.AsyncClient, broker: Broker, deliveries, consumer
) -> None:
    """At-least-once delivery is the contract; processed_events is the answer."""
    accounts = [await register_account(client) for _ in range(3)]
    envelope = trip_completed(participants=[a.id for a in accounts])

    await broker.publish(envelope)
    await _until(lambda: pending_count(app), "the rating window never opened")
    await broker.publish(envelope)
    # The duplicate is acknowledged like any success — it is not an error, and
    # dead-lettering it would fill the DLQ with messages nobody will act on.
    await _until(lambda: deliveries.acked == 2, "the redelivery was never acknowledged")
    assert deliveries.dead_lettered == 0

    assert await pending_count(app) == 6


async def test_a_body_that_is_not_an_envelope_is_dead_lettered(
    broker: Broker, deliveries, consumer
) -> None:
    """Never requeued: a poison message at the head of the queue is a spin."""
    await broker.publish_raw(b"{not json", routing_key="trip.completed")

    # Both halves matter: nacked without requeue, and actually routed by the
    # queue's dead-letter exchange to the DLQ rather than dropped.
    await _until(lambda: depth_is(broker, DLQ, 1), "the message never reached the DLQ")
    assert (deliveries.dead_lettered, deliveries.requeued, deliveries.acked) == (1, 0, 0)
    assert await broker.depth(QUEUE) == 0


async def test_an_unknown_event_type_is_acked_not_dead_lettered(
    app: FastAPI, broker: Broker, deliveries, consumer
) -> None:
    """A message for a newer build is absorbed; the DLQ is for real failures.

    Published on `trip.completed`, because that is the only routing key bound
    to this queue and the envelope has to actually arrive to be ignored. The
    disagreement between the routing key and the envelope's `event_type` is
    the shape this really takes: a binding widened, or a publisher emitting
    something this build has never heard of.
    """
    await broker.publish_raw(
        json.dumps(
            {
                "event_id": str(uuid.uuid4()),
                "event_type": "trip.teleported",
                "event_version": 1,
                "occurred_at": dt.datetime.now(dt.UTC)
                .isoformat()
                .replace("+00:00", "Z"),
                "aggregate_id": str(uuid.uuid4()),
                "payload": {},
            }
        ).encode(),
        routing_key="trip.completed",
    )

    await _until(lambda: deliveries.acked == 1, "the message was never acknowledged")
    assert deliveries.dead_lettered == 0
    assert await broker.depth(DLQ) == 0
    assert await pending_count(app) == 0
