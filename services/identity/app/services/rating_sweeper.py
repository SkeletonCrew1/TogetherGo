"""The nightly close-out of rating windows nobody used.

Housekeeping, and only housekeeping. Every read of `pending_ratings` already
filters on `expires_at`, so a sweep that never ran would not change a single
answer this service gives — what it buys is a table and a partial index that
stay the size of the work actually outstanding, instead of growing by
`n * (n - 1)` rows per completed trip forever.

An asyncio task inside the service, like the outbox relay, rather than a cron
container: the work is one bounded UPDATE, and a second deployable to run it
would be more moving parts than the job is worth. Two replicas running it at
the same time is harmless — `SKIP LOCKED` means they take different chunks
and the UPDATE is idempotent besides.
"""

from __future__ import annotations

import asyncio
import logging

from sqlalchemy.ext.asyncio import AsyncSession, async_sessionmaker

from app.services.rating_service import RatingService

logger = logging.getLogger(__name__)

# One chunk per transaction, repeated until a short one says the backlog is
# gone. Bounded so the sweep never takes a long lock on a table the submission
# path also writes.
SWEEP_CHUNK = 5_000


class PendingRatingSweeper:
    def __init__(
        self,
        *,
        sessionmaker: async_sessionmaker[AsyncSession],
        interval: float,
    ) -> None:
        self._sessionmaker = sessionmaker
        self._interval = interval
        self._task: asyncio.Task[None] | None = None
        self._stopping = asyncio.Event()

    async def start(self) -> None:
        self._stopping.clear()
        self._task = asyncio.create_task(self._run(), name="pending-rating-sweeper")

    async def stop(self) -> None:
        self._stopping.set()
        if self._task is not None:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
            self._task = None

    async def _run(self) -> None:
        # Sleeps first. A process that restarts often would otherwise sweep on
        # every boot, and there is nothing urgent here — the rows it closes
        # have already stopped being visible to every reader.
        while not self._stopping.is_set():
            await self._sleep(self._interval)
            if self._stopping.is_set():
                return
            try:
                closed = await self.sweep()
                if closed:
                    logger.info("rating windows expired", extra={"count": closed})
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                # The rows are still there and still expired; the next pass
                # picks them up. Nothing is lost by swallowing this.
                logger.warning(
                    "rating window sweep failed",
                    extra={"error_type": type(exc).__name__, "error": str(exc)},
                )

    async def sweep(self) -> int:
        """Close every window past its expiry. Returns how many.

        Public and callable on its own so a test — or an operator — can force
        a pass without waiting for the interval.
        """
        total = 0
        while True:
            async with self._sessionmaker() as session:
                closed = await RatingService(session).expire_windows(limit=SWEEP_CHUNK)
            total += closed
            if closed < SWEEP_CHUNK:
                return total

    async def _sleep(self, seconds: float) -> None:
        try:
            await asyncio.wait_for(self._stopping.wait(), timeout=seconds)
        except TimeoutError:
            pass
