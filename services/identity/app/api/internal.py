"""GET /internal/users — the batch resolver every other service calls.

This endpoint is not routed through the gateway. `deploy/traefik/dynamic.yml`
routes `/api/auth`, `/api/users` and `/.well-known` to this service and
nothing else, so `/internal/*` is a 404 from Traefik and never reaches the
application at all. `tests/test_gateway_routes.py` asserts that. The
`X-Internal-Token` check below is the second line of defence behind it, since
in Compose every service shares one network.

Three properties of this endpoint are contract, not implementation, because
the trip service is built against them:

* **Batch only.** There is deliberately no single-id variant. Offering one
  guarantees it gets called from inside a loop over a trip's roster, and the
  N+1 would be nobody's fault in particular.
* **Unknown ids are omitted, not an error.** A trip whose member deleted
  their account still renders; the caller shows a placeholder for the id it
  did not get back.
* **The response is small and stable.** Adding a field here means updating
  `contracts/openapi/identity.yaml` in the same change.
"""

from __future__ import annotations

import uuid

from fastapi import APIRouter, Depends, Response

from app.deps import get_profile_service, internal_user_ids, require_internal_token
from app.schemas import ErrorResponse, InternalUser, InternalUsersResponse
from app.schemas.internal import MAX_INTERNAL_IDS
from app.services.profile_service import ProfileService

# How long a caller may reuse a resolved batch. Sixty seconds is the documented
# TTL every consumer aligns its own cache with, so a display name that changes
# is stale platform-wide for a bounded and known interval rather than for
# however long each service happened to pick.
CACHE_MAX_AGE_SECONDS = 60

router = APIRouter(
    prefix="/internal",
    tags=["internal"],
    dependencies=[Depends(require_internal_token)],
)


@router.get(
    "/users",
    response_model=InternalUsersResponse,
    summary=f"Resolve up to {MAX_INTERNAL_IDS} user ids to display data",
    response_description=(
        "The users that exist among the given ids, in no guaranteed order. "
        "Ids that matched nothing are omitted; render a placeholder for them. "
        f"Cache-Control: private, max-age={CACHE_MAX_AGE_SECONDS}."
    ),
    responses={
        400: {
            "model": ErrorResponse,
            "description": (
                f"`invalid_ids` — `ids` was empty or not all uuids. "
                f"`too_many_ids` — more than {MAX_INTERNAL_IDS} ids in one call."
            ),
        },
        401: {
            "model": ErrorResponse,
            "description": "`invalid_internal_token` — the header is missing or wrong.",
        },
    },
)
async def resolve_users(
    response: Response,
    user_ids: list[uuid.UUID] = Depends(internal_user_ids),
    service: ProfileService = Depends(get_profile_service),
) -> InternalUsersResponse:
    users = await service.resolve_users(user_ids)
    # `private` because the response is per-caller and must not be held by a
    # shared proxy; the gateway does not see this route, but a future sidecar
    # or client-side HTTP cache would.
    response.headers["Cache-Control"] = f"private, max-age={CACHE_MAX_AGE_SECONDS}"
    return InternalUsersResponse(
        users=[InternalUser.from_user(user, rating) for user, rating in users]
    )
