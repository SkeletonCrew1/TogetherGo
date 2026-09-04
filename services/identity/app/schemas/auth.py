"""Request and response models for the auth endpoints.

Every rule that can be expressed as a property of the request body lives
here, not in a router and not in a service: age eligibility, password
strength and the password/email relationship are all validation, and the
router's job is to hand an already-valid model to the service.
"""

from __future__ import annotations

import datetime as dt

from pydantic import BaseModel, EmailStr, Field, ValidationInfo, field_validator

from app.schemas.password_policy import (
    ExistingPassword,
    NewPassword,
    contains_email_local_part,
    reject_all_digits,
)
from app.schemas.user import PrivateProfile, age_on

MINIMUM_AGE_YEARS = 16
MAXIMUM_AGE_YEARS = 120


class RegisterRequest(BaseModel):
    email: EmailStr
    password: NewPassword
    # Republished as the event catalogue's `display_name`, which is 1–64
    # characters — so the bound belongs at the point of entry.
    full_name: str = Field(min_length=1, max_length=64)
    birth_date: dt.date
    bio: str | None = Field(default=None, max_length=500)

    @field_validator("full_name")
    @classmethod
    def _strip_full_name(cls, value: str) -> str:
        stripped = value.strip()
        if not stripped:
            raise ValueError("must not be blank")
        return stripped

    @field_validator("birth_date")
    @classmethod
    def _old_enough(cls, value: dt.date) -> dt.date:
        today = dt.datetime.now(dt.UTC).date()
        if value > today:
            raise ValueError("must not be in the future")
        age = age_on(value, today)
        if age < MINIMUM_AGE_YEARS:
            raise ValueError(f"the account holder must be at least {MINIMUM_AGE_YEARS} years old")
        if age > MAXIMUM_AGE_YEARS:
            raise ValueError("is not a plausible date of birth")
        return value

    @field_validator("password")
    @classmethod
    def _password_strength(cls, value: str, info: ValidationInfo) -> str:
        reject_all_digits(value)

        # `email` is declared before `password`, so it has already been
        # validated and is available here. Doing this as a field validator
        # rather than a model validator is what puts the error on the
        # `password` field instead of on the body as a whole. The password
        # change endpoint has no email in its body and applies the same rule
        # from the service instead.
        email = info.data.get("email")
        if email is not None and contains_email_local_part(value, str(email)):
            raise ValueError("must not contain the local part of the email address")

        return value


class LoginRequest(BaseModel):
    email: EmailStr
    # No strength rules here: an old account may predate them, and the only
    # question login asks is whether the value matches.
    password: ExistingPassword


class RefreshRequest(BaseModel):
    refresh_token: str = Field(min_length=1, max_length=512)


class LogoutRequest(BaseModel):
    refresh_token: str = Field(min_length=1, max_length=512)


class TokenPair(BaseModel):
    access_token: str
    refresh_token: str
    expires_in: int


class RefreshResponse(TokenPair):
    """POST /api/auth/refresh — new pair, no profile."""


class AuthResponse(TokenPair):
    """POST /api/auth/register and /login — pair plus the account."""

    user: PrivateProfile
