"""Registration, login, rotation and logout.

All the business rules live here. Routers translate HTTP into a validated
Pydantic model and back again; they make no decisions.
"""

from __future__ import annotations

import asyncio
import datetime as dt
import logging
import uuid
from dataclasses import dataclass

from sqlalchemy.exc import IntegrityError
from sqlalchemy.ext.asyncio import AsyncSession

from app.errors import InvalidCredentials, InvalidRefreshToken, RegistrationConflict
from app.events.envelope import USER_REGISTERED, rfc3339
from app.models import User
from app.repositories import (
    Aggregate,
    OutboxRepository,
    RatingRepository,
    RefreshTokenRepository,
    UserRepository,
)
from app.schemas.auth import LoginRequest, RegisterRequest
from app.services import passwords
from app.services.tokens import (
    TokenIssuer,
    generate_refresh_token,
    generate_verification_token,
    hash_refresh_token,
)

logger = logging.getLogger(__name__)

UNIQUE_VIOLATION = "23505"
# Long enough for any real browser string, short enough that the column
# cannot be used as free storage by a hostile client.
USER_AGENT_MAX_LENGTH = 512


@dataclass(frozen=True)
class IssuedTokens:
    access_token: str
    refresh_token: str
    expires_in: int


@dataclass(frozen=True)
class AuthResult:
    user: User
    tokens: IssuedTokens
    # The account's rating aggregate, carried alongside the row because every
    # `PrivateProfile` shows it. A fresh registration has none by definition
    # and does not go looking for one.
    rating: Aggregate = Aggregate.empty()


class AuthService:
    def __init__(
        self,
        session: AsyncSession,
        *,
        issuer: TokenIssuer,
        refresh_token_ttl: int,
    ) -> None:
        self._session = session
        self._users = UserRepository(session)
        self._refresh_tokens = RefreshTokenRepository(session)
        self._outbox = OutboxRepository(session)
        self._issuer = issuer
        self._refresh_token_ttl = refresh_token_ttl

    # -- registration -------------------------------------------------------

    async def register(self, data: RegisterRequest, *, user_agent: str | None) -> AuthResult:
        """Create the account, stage `user.registered`, and log the user in.

        The user row, the outbox row and the first refresh token are one
        transaction. There is no window in which the account exists but the
        event does not, which is the whole point of the outbox.
        """
        # Argon2id is ~50 ms of CPU. Doing it inline would stall the event
        # loop for every other in-flight request; the C code releases the GIL,
        # so a worker thread genuinely runs in parallel.
        password_hash = await asyncio.to_thread(passwords.hash_password, data.password)

        user_id = uuid.uuid4()
        email = str(data.email)

        try:
            async with self._session.begin():
                user = await self._users.create(
                    user_id=user_id,
                    email=email,
                    password_hash=password_hash,
                    full_name=data.full_name,
                    birth_date=data.birth_date,
                    bio=data.bio,
                )

                await self._outbox.enqueue(
                    aggregate_id=user.id,
                    event_type=USER_REGISTERED,
                    payload={
                        "user_id": str(user.id),
                        "email": user.email,
                        "display_name": user.full_name,
                        # Single-use and the only credential-ish value on the
                        # bus. Not persisted here: consuming it belongs to the
                        # email-verification flow, which does not exist yet.
                        "verification_token": generate_verification_token(),
                        "registered_at": rfc3339(user.created_at),
                    },
                )

                tokens = await self._issue_pair(user, user_agent=user_agent)
        except IntegrityError as exc:
            if _is_unique_violation(exc):
                # Deliberately does not say which constraint failed. A caller
                # probing for registered addresses learns nothing from it.
                logger.info("registration rejected as conflicting")
                raise RegistrationConflict() from exc
            raise

        logger.info("user registered", extra={"user_id": str(user.id)})
        # No rating lookup: an account created a microsecond ago has not been
        # rated, and a query to confirm that would be a round trip for a
        # foregone conclusion.
        return AuthResult(user=user, tokens=tokens)

    # -- login --------------------------------------------------------------

    async def login(self, data: LoginRequest, *, user_agent: str | None) -> AuthResult:
        """Authenticate, or fail in a way that reveals nothing.

        Unknown address, wrong password and deactivated account all raise the
        same error, and all three do exactly one Argon2id verification, so
        neither the body nor the timing distinguishes them.
        """
        user = await self._users.get_by_email(str(data.email))

        if user is None:
            await asyncio.to_thread(passwords.verify_dummy, data.password)
            raise InvalidCredentials()

        valid = await asyncio.to_thread(passwords.verify_password, user.password_hash, data.password)
        if not valid:
            raise InvalidCredentials()

        # Checked after the verification, not before: an early return here
        # would make a deactivated account answer faster than a live one.
        if not user.is_active:
            logger.info("login refused for inactive account", extra={"user_id": str(user.id)})
            raise InvalidCredentials()

        # No `session.begin()` here: the lookup above already autobegan a
        # transaction, and SQLAlchemy refuses to start a second one on the
        # same session. The writes below join that transaction and commit
        # with it, which is the atomicity we want anyway.
        if passwords.needs_rehash(user.password_hash):
            # The password is in hand and already verified; this is the only
            # moment the cost parameters can be upgraded.
            rehashed = await asyncio.to_thread(passwords.hash_password, data.password)
            await self._users.update_password_hash(user, rehashed)

        tokens = await self._issue_pair(user, user_agent=user_agent)
        await self._session.commit()

        logger.info("user logged in", extra={"user_id": str(user.id)})
        rating = await RatingRepository(self._session).get_aggregate(user.id)
        return AuthResult(user=user, tokens=tokens, rating=rating)

    # -- rotation -----------------------------------------------------------

    async def refresh(self, raw_token: str, *, user_agent: str | None) -> IssuedTokens:
        """Exchange a refresh token for a new pair, detecting replays.

        A token that has already been rotated away is either a stolen copy
        being replayed or the legitimate holder's copy after a thief used it.
        There is no way to tell which, so both are treated as a compromise:
        every live token for the account dies and the user re-authenticates.
        """
        token_hash = hash_refresh_token(raw_token)
        now = dt.datetime.now(dt.UTC)
        reuse_detected_for: uuid.UUID | None = None
        issued: IssuedTokens | None = None
        rotated_for: uuid.UUID | None = None

        async with self._session.begin():
            row = await self._refresh_tokens.get_by_hash_for_update(token_hash)
            if row is None:
                raise InvalidRefreshToken()

            if row.revoked_at is not None and row.replaced_by is not None:
                # Revoked *and* rotated away: someone is presenting a token
                # that already has a successor. Either a thief is replaying a
                # stolen copy, or the legitimate holder is presenting theirs
                # after a thief already spent it. There is no way to tell
                # which, so both are treated as a compromise.
                killed = await self._refresh_tokens.revoke_all_for_user(row.user_id, now=now)
                reuse_detected_for = row.user_id
                logger.warning(
                    "refresh token reuse detected; revoking every session",
                    extra={"user_id": str(row.user_id), "sessions_revoked": killed},
                )
                # Fall through: the revocation has to commit before we can
                # answer, so the 401 is raised once this block has closed.
            elif row.revoked_at is not None:
                # Revoked with no successor: the session was ended on purpose
                # — logout, or an earlier reuse sweep. Presenting it again is
                # an ordinary stale-token 401, not evidence of theft. Without
                # this branch, one client refreshing after logout would look
                # like a replay and sign the account out everywhere.
                raise InvalidRefreshToken()
            else:
                if row.expires_at <= now:
                    raise InvalidRefreshToken()

                user = await self._users.get_by_id(row.user_id)
                if user is None or not user.is_active:
                    raise InvalidRefreshToken()

                raw_new, new_hash = generate_refresh_token()
                access = self._issuer.issue_access_token(user_id=user.id, email=user.email)
                new_row = await self._refresh_tokens.create(
                    user_id=user.id,
                    token_hash=new_hash,
                    expires_at=now + dt.timedelta(seconds=self._refresh_token_ttl),
                    user_agent=_truncate_user_agent(user_agent),
                    access_token_jti=access.jti,
                )
                await self._refresh_tokens.rotate(old=row, new_id=new_row.id, now=now)

                issued = IssuedTokens(
                    access_token=access.token,
                    refresh_token=raw_new,
                    expires_in=access.expires_in,
                )
                rotated_for = user.id

        if reuse_detected_for is not None or issued is None:
            raise InvalidRefreshToken()

        logger.info("refresh token rotated", extra={"user_id": str(rotated_for)})
        return issued

    # -- logout -------------------------------------------------------------

    async def logout(self, raw_token: str) -> None:
        """Revoke the whole rotation chain the token belongs to.

        One chain is one login session, so logging out on a laptop does not
        sign the account out on a phone. Unknown tokens are a silent no-op:
        the endpoint answers 204 either way and is not an existence oracle.
        """
        revoked = await self._refresh_tokens.revoke_chain(
            hash_refresh_token(raw_token), now=dt.datetime.now(dt.UTC)
        )
        await self._session.commit()
        logger.info("logout processed", extra={"tokens_revoked": revoked})

    # -- shared -------------------------------------------------------------

    async def _issue_pair(self, user: User, *, user_agent: str | None) -> IssuedTokens:
        """Mint an access token and persist a fresh refresh token.

        Called inside the caller's transaction — the refresh token row must
        commit with whatever else that transaction is doing.

        The two are minted together and the row remembers the access token's
        `jti`. That link is what `POST /api/users/me/password` uses to revoke
        every session except the one that asked.
        """
        raw_refresh, token_hash = generate_refresh_token()
        access = self._issuer.issue_access_token(user_id=user.id, email=user.email)
        await self._refresh_tokens.create(
            user_id=user.id,
            token_hash=token_hash,
            expires_at=dt.datetime.now(dt.UTC) + dt.timedelta(seconds=self._refresh_token_ttl),
            user_agent=_truncate_user_agent(user_agent),
            access_token_jti=access.jti,
        )
        return IssuedTokens(
            access_token=access.token,
            refresh_token=raw_refresh,
            expires_in=access.expires_in,
        )


def _is_unique_violation(exc: IntegrityError) -> bool:
    return getattr(exc.orig, "sqlstate", None) == UNIQUE_VIOLATION


def _truncate_user_agent(value: str | None) -> str | None:
    if value is None:
        return None
    return value[:USER_AGENT_MAX_LENGTH]

