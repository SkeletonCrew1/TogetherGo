"""The password rules, in one place.

Registration and password change apply the same policy, and a rule that lived
in only one of them would be a rule a user could route around by signing up
with a weak password's stronger twin and then changing it.

Two of the three rules are properties of the value alone and belong in a
Pydantic validator. The third — "must not contain the local part of the email
address" — needs the account, which a password-change body does not carry, so
it is exposed as a function the service calls with the email in hand.
"""

from __future__ import annotations

from typing import Annotated

from pydantic import Field

MINIMUM_PASSWORD_LENGTH = 10
# Argon2id happily hashes megabytes, and an endpoint that does is a
# denial-of-service lever. Nothing legitimate needs more than this.
MAXIMUM_PASSWORD_LENGTH = 256

# The shape of every field that carries a *new* password. A field carrying an
# existing one (login, `current_password`) uses min_length=1 instead: an old
# account may predate the rules and the only question being asked is whether
# the value matches.
NewPassword = Annotated[
    str, Field(min_length=MINIMUM_PASSWORD_LENGTH, max_length=MAXIMUM_PASSWORD_LENGTH)
]
ExistingPassword = Annotated[str, Field(min_length=1, max_length=MAXIMUM_PASSWORD_LENGTH)]

# A one- or two-character local part turns up inside almost any password by
# coincidence; rejecting on it would be noise, not security.
MINIMUM_MEANINGFUL_LOCAL_PART = 3


def reject_all_digits(value: str) -> str:
    """Validator body for "must not consist only of digits".

    `str.isdigit` is true for superscripts and other unicode digits too, which
    is the behaviour we want: "²²²²²²²²²²" is no better a password than
    "1111111111".
    """
    if value.isdigit():
        raise ValueError("must not consist only of digits")
    return value


def contains_email_local_part(password: str, email: str) -> bool:
    """Whether the password embeds the account's own address."""
    local_part = email.split("@", 1)[0]
    if len(local_part) < MINIMUM_MEANINGFUL_LOCAL_PART:
        return False
    return local_part.lower() in password.lower()
