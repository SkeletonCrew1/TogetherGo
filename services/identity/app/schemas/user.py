"""The three shapes a user is ever returned in, and the bodies that change one.

There are exactly three, and which fields each carries is the whole point:

* `PrivateProfile` — what the account holder sees about themselves. Email,
  phone, birth date, `is_active`.
* `PublicProfile` — what any authenticated caller sees about someone else.
  No email, no phone, and no birth date: `age` is derived from it server-side
  so that a date of birth never crosses the wire.
* `InternalUser` — what other services get from the batch resolver. Four
  display fields and nothing else; see `app/api/internal.py`.

Anything added to one of these is added to `contracts/openapi/identity.yaml`
in the same change (`make openapi-identity`).
"""

from __future__ import annotations

import datetime as dt
import uuid
from typing import TYPE_CHECKING, Self
from urllib.parse import urlparse

from pydantic import BaseModel, EmailStr, Field, field_validator, model_validator

from app.schemas.password_policy import ExistingPassword, NewPassword, reject_all_digits

if TYPE_CHECKING:  # pragma: no cover - import for typing only
    # Schemas describe the wire, models describe the table. The builders below
    # read a model instance, but nothing here should ever import one at
    # runtime and start reaching into the ORM.
    from app.models import User
    from app.repositories.ratings import Aggregate

FULL_NAME_MAX_LENGTH = 64
BIO_MAX_LENGTH = 500
# Long enough for a signed CDN URL, short enough that the column is not free
# storage for a hostile client.
PHOTO_URL_MAX_LENGTH = 2048
PHOTO_URL_SCHEMES = frozenset({"http", "https"})


def age_on(birth_date: dt.date, today: dt.date) -> int:
    """Completed years between the two dates."""
    return today.year - birth_date.year - (
        (today.month, today.day) < (birth_date.month, birth_date.day)
    )


def _age_of(user: User) -> int | None:
    """The user's age today, or None if they have no birth date on file.

    Computed here on every read rather than stored: an age column would be
    wrong for a fraction of its holders every single day.
    """
    if user.birth_date is None:
        return None
    return age_on(user.birth_date, dt.datetime.now(dt.UTC).date())


def _validate_photo_url(value: str) -> str:
    """An absolute http(s) URL, kept exactly as the caller sent it.

    Deliberately not `pydantic.HttpUrl`: that type normalises what it parses
    (`https://cdn.example.com` comes back as `https://cdn.example.com/`), and
    a PATCH that quietly returns something other than what was sent is a
    surprise no caller asked for.
    """
    parsed = urlparse(value)
    if parsed.scheme not in PHOTO_URL_SCHEMES or not parsed.netloc:
        raise ValueError("must be an absolute http(s) URL")
    return value


# E.164: a leading +, a non-zero country code, up to 15 digits in total. The
# strictest thing that is still true of every real phone number, and it means
# the notification service never has to guess at a format.
PHONE_PATTERN = r"^\+[1-9]\d{6,14}$"


# --- responses --------------------------------------------------------------


class PrivateProfile(BaseModel):
    """GET /api/users/me, and the `user` of an auth response.

    The only shape that carries email, phone and birth date. Never returned
    for anyone but the authenticated caller themselves.
    """

    id: uuid.UUID
    email: EmailStr
    full_name: str
    bio: str | None = None
    birth_date: dt.date | None = None
    age: int | None = None
    phone: str | None = None
    photo_url: str | None = None
    rating_avg: float | None = None
    rating_count: int
    is_active: bool
    created_at: dt.datetime
    updated_at: dt.datetime

    @classmethod
    def from_user(cls, user: User, rating: Aggregate) -> Self:
        return cls(
            id=user.id,
            email=user.email,
            full_name=user.full_name,
            bio=user.bio,
            birth_date=user.birth_date,
            age=_age_of(user),
            phone=user.phone,
            photo_url=user.photo_url,
            rating_avg=rating.average(),
            rating_count=rating.rating_count,
            is_active=user.is_active,
            created_at=user.created_at,
            updated_at=user.updated_at,
        )


class PublicProfile(BaseModel):
    """GET /api/users/{id} — what one traveller may see about another.

    Email and phone are absent from the model, not merely unset, so there is
    no code path that can fill them in. `birth_date` is absent for the same
    reason: `age` is the only thing a co-traveller needs, and it is derived
    from the date rather than exposing it.
    """

    id: uuid.UUID
    full_name: str
    bio: str | None = None
    photo_url: str | None = None
    age: int | None = None
    rating_avg: float | None = None
    rating_count: int
    created_at: dt.datetime

    @classmethod
    def from_user(cls, user: User, rating: Aggregate) -> Self:
        return cls(
            id=user.id,
            full_name=user.full_name,
            bio=user.bio,
            photo_url=user.photo_url,
            age=_age_of(user),
            rating_avg=rating.average(),
            rating_count=rating.rating_count,
            created_at=user.created_at,
        )


class InternalUser(BaseModel):
    """One entry of GET /internal/users.

    Five fields, and the shape other services build their user projections
    from. Adding one here is a contract change: regenerate
    `contracts/openapi/identity.yaml` in the same commit.
    """

    id: uuid.UUID
    full_name: str
    photo_url: str | None = None
    rating_avg: float | None = None
    rating_count: int

    @classmethod
    def from_user(cls, user: User, rating: Aggregate) -> Self:
        return cls(
            id=user.id,
            full_name=user.full_name,
            photo_url=user.photo_url,
            rating_avg=rating.average(),
            rating_count=rating.rating_count,
        )


class InternalUsersResponse(BaseModel):
    """`{"users": [...]}` — an object, not a bare array.

    Ids that matched nothing are simply absent, so the list is not positional
    and callers must key it by `id`. Wrapping it leaves room to add a
    sibling field later without breaking every caller's parser.
    """

    users: list[InternalUser]


# --- requests ---------------------------------------------------------------


class UpdateProfileRequest(BaseModel):
    """PATCH /api/users/me.

    Every field is optional and absence means "leave it alone". For the three
    nullable ones an explicit `null` is a real instruction — clear the value —
    which is why the service works from `model_fields_set` and not from a
    dict of non-None values.
    """

    full_name: str | None = Field(default=None, min_length=1, max_length=FULL_NAME_MAX_LENGTH)
    bio: str | None = Field(default=None, max_length=BIO_MAX_LENGTH)
    phone: str | None = Field(default=None, pattern=PHONE_PATTERN)
    photo_url: str | None = Field(default=None, max_length=PHOTO_URL_MAX_LENGTH)

    @field_validator("full_name")
    @classmethod
    def _strip_full_name(cls, value: str | None) -> str | None:
        if value is None:
            return None
        stripped = value.strip()
        if not stripped:
            raise ValueError("must not be blank")
        return stripped

    @field_validator("photo_url")
    @classmethod
    def _absolute_http_url(cls, value: str | None) -> str | None:
        return None if value is None else _validate_photo_url(value)

    @model_validator(mode="after")
    def _full_name_is_not_nullable(self) -> Self:
        # `full_name` is NOT NULL in the database, so `{"full_name": null}` is
        # not "clear it", it is a request that cannot be honoured. It has to be
        # caught at the model level: by the time a field validator sees None it
        # can no longer tell an explicit null from an omitted key.
        if "full_name" in self.model_fields_set and self.full_name is None:
            raise ValueError("full_name must not be null")
        return self

    def changes(self) -> dict[str, str | None]:
        """The fields the caller actually sent, null included."""
        return {name: getattr(self, name) for name in sorted(self.model_fields_set)}


class ChangePasswordRequest(BaseModel):
    """POST /api/users/me/password."""

    # No strength rules on the current one — the account may predate them and
    # the only question is whether it matches.
    current_password: ExistingPassword
    new_password: NewPassword

    @field_validator("new_password")
    @classmethod
    def _password_strength(cls, value: str) -> str:
        # The "must not contain the local part of the email address" rule
        # needs the account, which this body does not carry; the service
        # applies it once it has the user in hand.
        return reject_all_digits(value)

    @model_validator(mode="after")
    def _actually_a_change(self) -> Self:
        if self.current_password == self.new_password:
            raise ValueError("new_password must differ from current_password")
        return self
