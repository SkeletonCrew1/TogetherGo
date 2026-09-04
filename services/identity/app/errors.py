"""Domain errors and the single API error shape.

Every error response the service produces is

    {"error": {"code": "...", "message": "...", "details": {...}}}

HTTP status carries the class of failure, `code` the specific reason. Domain
code raises these exceptions; mapping them onto responses happens in exactly
one place, `install_handlers`.
"""

from __future__ import annotations

import logging
from typing import Any

from fastapi import FastAPI, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse
from starlette.exceptions import HTTPException as StarletteHTTPException

logger = logging.getLogger(__name__)


class DomainError(Exception):
    """Base for everything that maps onto a client-visible error."""

    status_code: int = 500
    code: str = "internal_error"
    message: str = "An unexpected error occurred."

    def __init__(
        self,
        message: str | None = None,
        *,
        details: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> None:
        super().__init__(message or self.message)
        self.message = message or self.message
        self.details = details
        self.headers = headers


class InvalidCredentials(DomainError):
    """Wrong password, unknown email, or a deactivated account.

    One exception for all three on purpose — see `AuthService.login`.
    """

    status_code = 401
    code = "invalid_credentials"
    message = "Invalid email or password."


class InvalidRefreshToken(DomainError):
    """Unknown, expired, revoked or replayed refresh token.

    Reuse detection uses this same error: an attacker holding a stolen token
    learns only that it stopped working, not that the replay was noticed.
    """

    status_code = 401
    code = "invalid_refresh_token"
    message = "The refresh token is invalid or has expired."


class InvalidAccessToken(DomainError):
    """Missing, malformed, expired or wrong-type bearer token.

    One error for every reason a token did not work. Telling a caller that a
    token was well-formed but expired, versus not a token at all, is help an
    attacker probing the endpoint can use and a legitimate client never needs
    — it refreshes on any 401 either way.
    """

    status_code = 401
    code = "invalid_access_token"
    message = "The access token is missing or invalid."

    def __init__(self, message: str | None = None) -> None:
        # RFC 6750: a 401 on a bearer-protected resource says so.
        super().__init__(message, headers={"WWW-Authenticate": "Bearer"})


class InvalidInternalToken(DomainError):
    """X-Internal-Token missing or wrong on an /internal route.

    Second line of defence only. /internal is not routed through the gateway,
    so reaching one of these at all means the caller is already on the
    internal network.
    """

    status_code = 401
    code = "invalid_internal_token"
    message = "A valid X-Internal-Token header is required."


class UserNotFound(DomainError):
    status_code = 404
    code = "user_not_found"
    message = "No such user."


class InvalidCurrentPassword(DomainError):
    """`current_password` did not match on a password change.

    403 and not 401 on purpose: the access token is fine and the caller is
    authenticated. A 401 here would tell every client library to throw the
    session away and refresh, which is precisely the wrong reaction to a
    typo in a form field.
    """

    status_code = 403
    code = "invalid_current_password"
    message = "The current password is incorrect."


class WeakPassword(DomainError):
    """A new password that passed the body's own constraints but not the
    rules that need the account to evaluate — see `PasswordPolicy`."""

    status_code = 422
    code = "weak_password"
    message = "The new password is not acceptable."


class InvalidUserIds(DomainError):
    status_code = 400
    code = "invalid_ids"
    message = "The `ids` parameter must be a comma-separated list of uuids."


class TooManyUserIds(DomainError):
    """More ids than the batch resolver will answer in one call.

    400 rather than truncating the list: a caller that silently got 100 of
    the 150 users it asked for would render placeholders for the other 50 and
    never find out why.
    """

    status_code = 400
    code = "too_many_ids"
    message = "Too many ids in one request."


class RegistrationConflict(DomainError):
    """The account could not be created.

    Deliberately vague. A caller probing for registered addresses gets the
    same body whether the email is taken or the insert lost a race.
    """

    status_code = 409
    code = "registration_conflict"
    message = "The account could not be created."


class NotEligibleToRate(DomainError):
    """No open rating window for this (trip, rater, ratee).

    403 and not 404: the caller is authenticated and the trip may well
    exist — what they lack is permission to rate this person for it. The
    message is the same whether the trip is unknown to identity, the window
    has closed, or the two never travelled together, because distinguishing
    those would turn the endpoint into an oracle for "was X on trip Y".

    This is the *only* eligibility check. Identity never calls the trip
    service to answer it; `trip.completed` carries the roster precisely so
    that the rating window keeps working while trip is down.
    """

    status_code = 403
    code = "not_eligible"
    message = "You cannot rate this traveller for this trip."


class RatingAlreadySubmitted(DomainError):
    """A score already exists for this (trip, rater, ratee).

    A rating is immutable once given. 409 rather than a silent overwrite:
    the second request states something the server will not do, and a client
    that meant to change its mind needs to be told that it cannot.
    """

    status_code = 409
    code = "rating_already_submitted"
    message = "You have already rated this traveller for this trip."


class InvalidCursor(DomainError):
    """A `cursor` query parameter that this service did not issue."""

    status_code = 400
    code = "invalid_cursor"
    message = "The cursor is not valid; omit it to start from the first page."


class RateLimited(DomainError):
    status_code = 429
    code = "rate_limited"
    message = "Too many attempts. Try again later."

    def __init__(self, retry_after: int) -> None:
        super().__init__(headers={"Retry-After": str(retry_after)})
        self.retry_after = retry_after


class ServiceUnavailable(DomainError):
    status_code = 503
    code = "service_unavailable"
    message = "The service is not ready."


def error_body(code: str, message: str, details: dict[str, Any] | None = None) -> dict[str, Any]:
    error: dict[str, Any] = {"code": code, "message": message}
    if details is not None:
        error["details"] = details
    return {"error": error}


def install_handlers(app: FastAPI) -> None:
    @app.exception_handler(DomainError)
    async def _domain(_: Request, exc: DomainError) -> JSONResponse:
        return JSONResponse(
            status_code=exc.status_code,
            content=error_body(exc.code, exc.message, exc.details),
            headers=exc.headers,
        )

    @app.exception_handler(RequestValidationError)
    async def _validation(_: Request, exc: RequestValidationError) -> JSONResponse:
        # FastAPI's default 422 body is its own shape; rewrite it into ours.
        # `input` is dropped from every entry: for a registration body that
        # field is the plaintext password.
        fields = [
            {
                "field": ".".join(str(part) for part in err["loc"][1:]) or "body",
                "message": err["msg"],
            }
            for err in exc.errors()
        ]
        return JSONResponse(
            status_code=422,
            content=error_body("validation_error", "The request body is invalid.", {"fields": fields}),
        )

    @app.exception_handler(StarletteHTTPException)
    async def _http(_: Request, exc: StarletteHTTPException) -> JSONResponse:
        codes = {401: "unauthorized", 403: "forbidden", 404: "not_found", 405: "method_not_allowed"}
        return JSONResponse(
            status_code=exc.status_code,
            content=error_body(codes.get(exc.status_code, "http_error"), str(exc.detail)),
            headers=getattr(exc, "headers", None),
        )

    @app.exception_handler(Exception)
    async def _unhandled(_: Request, exc: Exception) -> JSONResponse:
        # The exception text may quote a request parameter, so it is logged
        # and never returned.
        logger.exception("unhandled exception", extra={"error_type": type(exc).__name__})
        return JSONResponse(
            status_code=500,
            content=error_body("internal_error", "An unexpected error occurred."),
        )
