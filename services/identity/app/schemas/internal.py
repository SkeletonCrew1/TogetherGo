"""The query shape of the internal batch resolver.

`GET /internal/users?ids=a,b,c` is the only way another service turns user
ids into display data, and it is batch-only on purpose: a single-id variant
would be used one id at a time from inside a loop over a trip's roster, and
the N+1 would be nobody's fault in particular.
"""

from __future__ import annotations

import uuid

from pydantic import BaseModel, Field

from app.errors import InvalidUserIds, TooManyUserIds

# One page of trip participants is far below this; a caller that needs more
# pages the request itself.
MAX_INTERNAL_IDS = 100
# 36 characters per uuid plus a separator, with slack for whitespace. Bounds
# the string before anything tries to split it, so a megabyte of commas is
# rejected by the parser rather than by the loop.
_MAX_IDS_LENGTH = MAX_INTERNAL_IDS * 40


class InternalUsersQuery(BaseModel):
    """`?ids=` — a comma-separated list of uuids."""

    ids: str = Field(min_length=1, max_length=_MAX_IDS_LENGTH)

    def user_ids(self) -> list[uuid.UUID]:
        """Parse and validate, in the order the contract specifies.

        The errors here are 400s from `app.errors`, not Pydantic
        `ValueError`s, because the contract for this endpoint says 400 and
        FastAPI would turn a validator's ValueError into a 422. The count is
        checked against the ids *as sent*, before duplicates are collapsed —
        a caller that asked for 101 things has a bug whether or not two of
        them were the same.
        """
        parts = [part.strip() for part in self.ids.split(",") if part.strip()]

        if not parts:
            raise InvalidUserIds("The `ids` parameter must name at least one user.")

        if len(parts) > MAX_INTERNAL_IDS:
            raise TooManyUserIds(
                f"At most {MAX_INTERNAL_IDS} ids per request; {len(parts)} were given.",
                details={"limit": MAX_INTERNAL_IDS, "received": len(parts)},
            )

        seen: dict[uuid.UUID, None] = {}
        for part in parts:
            try:
                seen[uuid.UUID(part)] = None
            except ValueError as exc:
                # The offending value is echoed back: this is a
                # service-to-service endpoint, and the caller is a colleague
                # debugging their own request, not an anonymous prober.
                raise InvalidUserIds(
                    f"{part!r} is not a valid uuid.", details={"id": part}
                ) from exc

        # dict preserves insertion order, so the caller's order survives
        # deduplication even though the response is keyed by id anyway.
        return list(seen)
