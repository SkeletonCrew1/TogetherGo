"""Declarative base shared by every table in identity_db."""

from __future__ import annotations

import datetime as dt

from sqlalchemy import DateTime
from sqlalchemy.orm import DeclarativeBase


class Base(DeclarativeBase):
    # Convention: every timestamp in the platform is `timestamptz` in UTC.
    # Pinning it here means no column can quietly become a naive `timestamp`
    # because someone omitted the explicit type.
    type_annotation_map = {dt.datetime: DateTime(timezone=True)}

    # Fetch server defaults (`now()`, `true`) with a RETURNING clause on the
    # INSERT rather than leaving the attribute unloaded. Registration answers
    # with the row it just wrote, and a second SELECT to learn its own
    # created_at would be a round trip for nothing.
    __mapper_args__ = {"eager_defaults": True}
