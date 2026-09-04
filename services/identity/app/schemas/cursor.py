"""The keyset cursor every list endpoint in this service hands out.

CLAUDE.md: list endpoints paginate by keyset, never by OFFSET. OFFSET makes
the database count and discard every row of every preceding page, and it
silently repeats or skips rows when something is inserted between two
requests — for a rating feed that means a traveller seeing the same comment
twice and never seeing another one at all.

The wire format is the trip service's, deliberately: base64url of
`RFC3339Nano|uuid`. Two services inventing two cursor formats for the same
kind of listing is a difference the SPA would have to care about.
"""

from __future__ import annotations

import base64
import binascii
import datetime as dt
import uuid
from typing import NamedTuple

from app.errors import InvalidCursor

# Neither half can contain it: RFC 3339 has no '|', and neither does a uuid.
_SEPARATOR = "|"

# A real cursor is 72 characters. Bounding the string before anything decodes
# it means a megabyte in the query string is refused by a length check rather
# than by an allocator.
MAX_CURSOR_LENGTH = 256


class Cursor(NamedTuple):
    """The position of the last row of a page, on (timestamp, id).

    Both halves are needed. A timestamp alone is not unique — two ratings can
    land in the same microsecond — so a cursor holding only it either skips
    rows (`<`) or repeats them forever (`<=`). The id makes the ordering
    total, and it is the tiebreaker the index carries so the comparison stays
    an index qual.
    """

    timestamp: dt.datetime
    id: uuid.UUID

    def encode(self) -> str:
        """The opaque string clients echo back.

        `isoformat()` keeps microseconds: timestamptz stores them, and a
        cursor truncated to the second would re-read every row sharing that
        second with the last row of the page. Base64 is not encryption and is
        not pretending to be — it says "this is ours, not yours" and keeps a
        '+' out of a query string.
        """
        raw = f"{self.timestamp.astimezone(dt.UTC).isoformat()}{_SEPARATOR}{self.id}"
        return base64.urlsafe_b64encode(raw.encode("utf-8")).decode("ascii").rstrip("=")


def decode_cursor(raw: str | None) -> Cursor | None:
    """Parse what `encode` produced; refuse everything else with a 400.

    Every step here is something the client controls — the length, the
    alphabet, the separator, the timestamp, the uuid — so none of them may
    index into a value an earlier step has not already proved is there.
    """
    if raw is None or raw == "":
        return None
    if len(raw) > MAX_CURSOR_LENGTH:
        raise InvalidCursor()

    padding = "=" * (-len(raw) % 4)
    try:
        decoded = base64.urlsafe_b64decode(raw + padding).decode("utf-8")
    except (binascii.Error, ValueError, UnicodeDecodeError) as exc:
        raise InvalidCursor() from exc

    timestamp_part, separator, id_part = decoded.partition(_SEPARATOR)
    if not separator:
        raise InvalidCursor()

    try:
        timestamp = dt.datetime.fromisoformat(timestamp_part)
        identifier = uuid.UUID(id_part)
    except ValueError as exc:
        raise InvalidCursor() from exc

    if timestamp.tzinfo is None:
        # Everything on the wire is UTC and carries an offset. A naive
        # timestamp is not something this service ever encoded.
        raise InvalidCursor()

    return Cursor(timestamp=timestamp, id=identifier)
