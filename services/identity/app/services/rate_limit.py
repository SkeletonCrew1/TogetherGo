"""Per-IP sliding-window rate limiting in Redis.

A sorted set per key holds one member per attempt, scored by timestamp. Each
check drops entries that fell out of the window, counts what is left, and
adds the current attempt only if it is under the limit — all inside one Lua
script, so the read-modify-write is atomic across replicas.

A fixed-window counter would be simpler and wrong in a specific way: it lets
a caller spend the whole budget at 14:59 and the whole budget again at 15:00.
"""

from __future__ import annotations

import logging
import math
import time
import uuid

from redis.asyncio import Redis
from redis.exceptions import RedisError

logger = logging.getLogger(__name__)

# KEYS[1] the window key. ARGV: now_ms, window_ms, limit, member.
# Returns {allowed, retry_after_ms}.
_SLIDING_WINDOW_LUA = """
local key       = KEYS[1]
local now_ms    = tonumber(ARGV[1])
local window_ms = tonumber(ARGV[2])
local limit     = tonumber(ARGV[3])
local member    = ARGV[4]

redis.call('ZREMRANGEBYSCORE', key, '-inf', now_ms - window_ms)

local used = redis.call('ZCARD', key)
if used >= limit then
    -- The window frees a slot when its oldest surviving attempt ages out.
    local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
    local retry_ms = window_ms - (now_ms - tonumber(oldest[2]))
    if retry_ms < 0 then retry_ms = 0 end
    return {0, retry_ms}
end

redis.call('ZADD', key, now_ms, member)
redis.call('PEXPIRE', key, window_ms)
return {1, 0}
"""


class RateLimiter:
    def __init__(self, redis: Redis, *, limit: int, window_seconds: int) -> None:
        self._redis = redis
        self._limit = limit
        self._window_ms = window_seconds * 1000
        self._script = redis.register_script(_SLIDING_WINDOW_LUA)

    async def check(self, scope: str, identifier: str) -> int | None:
        """Record an attempt. Returns None if allowed, else Retry-After seconds.

        Redis being unreachable fails **open**. A rate limiter is a control on
        abuse, not on correctness; losing it should degrade protection, not
        take registration and login offline for everyone.
        """
        key = f"ratelimit:{scope}:{identifier}"
        now_ms = int(time.time() * 1000)
        member = f"{now_ms}:{uuid.uuid4().hex[:8]}"

        try:
            allowed, retry_after_ms = await self._script(
                keys=[key],
                args=[now_ms, self._window_ms, self._limit, member],
            )
        except RedisError as exc:
            logger.warning(
                "rate limiter unavailable, allowing request",
                extra={"scope": scope, "error_type": type(exc).__name__},
            )
            return None

        if int(allowed) == 1:
            return None

        # Round up: a Retry-After that rounds down invites an immediate retry
        # that is still inside the window.
        return max(1, math.ceil(int(retry_after_ms) / 1000))
