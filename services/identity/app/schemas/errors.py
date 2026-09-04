"""The single API error shape, as a model, so it reaches the OpenAPI document.

`app.errors.install_handlers` builds these bodies by hand — it has to, since
it answers exceptions rather than returning from a route. This model is the
same shape declared for documentation, and the two are kept in step by
`tests/test_error_shape.py`.
"""

from __future__ import annotations

from typing import Any

from pydantic import BaseModel


class ErrorDetail(BaseModel):
    code: str
    message: str
    details: dict[str, Any] | None = None


class ErrorResponse(BaseModel):
    """`{"error": {"code": ..., "message": ..., "details": {...}}}`.

    HTTP status carries the class of failure, `code` the specific reason —
    clients branch on `code`, never on the message.
    """

    error: ErrorDetail
