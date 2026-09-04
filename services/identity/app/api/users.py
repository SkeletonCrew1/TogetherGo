"""GET/PATCH /api/users/me, POST /api/users/me/password, GET /api/users/{id}.

Routers hold no business logic: validate, delegate, shape the response.

The one thing this module *does* decide is which shape each response uses,
and that decision is the privacy boundary. `/me` answers with
`PrivateProfile`; anything about somebody else answers with `PublicProfile`,
a model that has no email or phone field to fill in even by accident.
"""

from __future__ import annotations

import uuid

from fastapi import APIRouter, Depends, Response, status

from app.deps import access_token_claims, current_user, get_profile_service
from app.models import User
from app.schemas import (
    ChangePasswordRequest,
    ErrorResponse,
    PrivateProfile,
    PublicProfile,
    UpdateProfileRequest,
)
from app.services.profile_service import ProfileService
from app.services.tokens import AccessTokenClaims

router = APIRouter(prefix="/api/users", tags=["users"])

UNAUTHENTICATED = {
    "model": ErrorResponse,
    "description": "`invalid_access_token` — the bearer token is missing or invalid.",
}
NOT_FOUND = {"model": ErrorResponse, "description": "`user_not_found`."}


@router.get(
    "/me",
    response_model=PrivateProfile,
    summary="The authenticated account's own profile",
    responses={401: UNAUTHENTICATED},
)
async def read_me(
    user: User = Depends(current_user),
    service: ProfileService = Depends(get_profile_service),
) -> PrivateProfile:
    return PrivateProfile.from_user(user, await service.rating_of(user.id))


@router.patch(
    "/me",
    response_model=PrivateProfile,
    summary="Edit the authenticated account's profile",
    response_description=(
        "The updated profile. Omitted fields are left alone; an explicit null "
        "clears bio, phone or photo_url."
    ),
    responses={401: UNAUTHENTICATED},
)
async def update_me(
    body: UpdateProfileRequest,
    user: User = Depends(current_user),
    service: ProfileService = Depends(get_profile_service),
) -> PrivateProfile:
    # Omitting a field leaves it alone; sending it as null clears it. The
    # service reads `model_fields_set` to tell those apart.
    updated = await service.update_profile(user, body)
    return PrivateProfile.from_user(updated, await service.rating_of(updated.id))


@router.post(
    "/me/password",
    status_code=status.HTTP_204_NO_CONTENT,
    response_class=Response,
    summary="Change the password and end every other session",
    responses={
        401: UNAUTHENTICATED,
        403: {
            "model": ErrorResponse,
            "description": "`invalid_current_password` — `current_password` did not match.",
        },
        422: {
            "model": ErrorResponse,
            "description": "`weak_password` — the new password fails the account's rules.",
        },
    },
)
async def change_password(
    body: ChangePasswordRequest,
    user: User = Depends(current_user),
    claims: AccessTokenClaims = Depends(access_token_claims),
    service: ProfileService = Depends(get_profile_service),
) -> Response:
    # The token's `jti` is how the service identifies the caller's own
    # session and spares it; see RefreshToken.access_token_jti.
    await service.change_password(user, body, access_token_jti=claims.jti)
    return Response(status_code=status.HTTP_204_NO_CONTENT)


@router.get(
    "/{user_id}",
    response_model=PublicProfile,
    summary="Another traveller's public profile",
    response_description="Public fields only — never email, phone or birth date.",
    responses={401: UNAUTHENTICATED, 404: NOT_FOUND},
)
async def read_user(
    user_id: uuid.UUID,
    _: User = Depends(current_user),
    service: ProfileService = Depends(get_profile_service),
) -> PublicProfile:
    # Declared after /me deliberately: routes match in declaration order, and
    # with this one first every request for /api/users/me would be a failed
    # attempt to parse "me" as a uuid.
    #
    # Authentication is required even though nothing here is private. It
    # keeps the endpoint from being an anonymous crawl of every account on
    # the platform, and a caller with no token has no trip to look at anyone
    # from.
    return PublicProfile.from_user(*await service.get_profile_view(user_id))
