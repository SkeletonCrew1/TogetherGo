"""Structured JSON logging to stdout.

Every line carries `service`, `level`, `msg`, `request_id` and — once the
request has been authenticated or the user is otherwise known — `user_id`.
Tokens, password hashes and request bodies never reach a log record: the
formatter only emits fields it was explicitly given.
"""

from __future__ import annotations

import contextvars
import datetime as dt
import json
import logging
import sys
from typing import Any

SERVICE_NAME = "identity"

request_id_var: contextvars.ContextVar[str | None] = contextvars.ContextVar(
    "request_id", default=None
)
user_id_var: contextvars.ContextVar[str | None] = contextvars.ContextVar(
    "user_id", default=None
)

# Attributes LogRecord always carries; anything else was put there by us via
# `extra=` and belongs in the JSON output.
_RESERVED = frozenset(vars(logging.LogRecord("", 0, "", 0, "", None, None))) | {
    "message",
    "asctime",
    "taskName",
    # uvicorn attaches an ANSI-coloured duplicate of the message; in JSON it
    # is noise with escape codes in it.
    "color_message",
}


class JSONFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, Any] = {
            "service": SERVICE_NAME,
            "level": record.levelname.lower(),
            "msg": record.getMessage(),
            "ts": dt.datetime.fromtimestamp(record.created, dt.UTC).isoformat(),
            "logger": record.name,
        }

        request_id = getattr(record, "request_id", None) or request_id_var.get()
        if request_id:
            payload["request_id"] = request_id

        user_id = getattr(record, "user_id", None) or user_id_var.get()
        if user_id:
            payload["user_id"] = user_id

        for key, value in record.__dict__.items():
            if key not in _RESERVED and key not in payload:
                payload[key] = value

        if record.exc_info:
            payload["error"] = self.formatException(record.exc_info)

        return json.dumps(payload, default=str, separators=(",", ":"))


def configure(level: str = "info") -> None:
    """Install the JSON formatter as the only stdout handler."""
    handler = logging.StreamHandler(sys.stdout)
    handler.setFormatter(JSONFormatter())

    root = logging.getLogger()
    root.handlers = [handler]
    root.setLevel(level.upper())

    # uvicorn installs its own colourised handlers before the app is built;
    # take them over so the container emits one log format rather than two.
    for name in ("uvicorn", "uvicorn.error"):
        logger = logging.getLogger(name)
        logger.handlers = []
        logger.propagate = True

    # uvicorn's access log is silenced outright rather than reformatted. The
    # request-context middleware already emits one structured line per
    # request with the request_id attached; letting this one through as well
    # would double every entry. Clearing its handlers is not enough — with
    # propagate on, the records would still reach the root handler.
    access = logging.getLogger("uvicorn.access")
    access.handlers = []
    access.propagate = False
    access.disabled = True
