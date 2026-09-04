"""POST /api/auth/{register,login,refresh,logout}.

Routers hold no business logic: validate, delegate, shape the response.
"""

from __future__ import annotations

from fastapi import APIRouter, Depends, Request, Response, status

from app.deps import get_auth_service, rate_limit_login, rate_limit_register
from app.schemas import (
    AuthResponse,
    LoginRequest,
    LogoutRequest,
    PrivateProfile,
    RefreshRequest,
    RefreshResponse,
    RegisterRequest,
)
from app.services.auth_service import AuthResult, AuthService

router = APIRouter(prefix="/api/auth", tags=["auth"])


def _auth_response(result: AuthResult) -> AuthResponse:
    return AuthResponse(
        user=PrivateProfile.from_user(result.user, result.rating),
        access_token=result.tokens.access_token,
        refresh_token=result.tokens.refresh_token,
        expires_in=result.tokens.expires_in,
    )


@router.post(
    "/register",
    response_model=AuthResponse,
    status_code=status.HTTP_201_CREATED,
    dependencies=[Depends(rate_limit_register)],
    summary="Create an account and sign in",
)
async def register(
    body: RegisterRequest,
    request: Request,
    service: AuthService = Depends(get_auth_service),
) -> AuthResponse:
    result = await service.register(body, user_agent=request.headers.get("user-agent"))
    return _auth_response(result)


@router.post(
    "/login",
    response_model=AuthResponse,
    dependencies=[Depends(rate_limit_login)],
    summary="Exchange credentials for a token pair",
)
async def login(
    body: LoginRequest,
    request: Request,
    service: AuthService = Depends(get_auth_service),
) -> AuthResponse:
    result = await service.login(body, user_agent=request.headers.get("user-agent"))
    return _auth_response(result)


@router.post(
    "/refresh",
    response_model=RefreshResponse,
    summary="Rotate a refresh token",
)
async def refresh(
    body: RefreshRequest,
    request: Request,
    service: AuthService = Depends(get_auth_service),
) -> RefreshResponse:
    # Not rate limited by IP: a legitimate client refreshes every 15 minutes
    # and the endpoint is already self-limiting — a wrong token is a 401, and
    # a replayed one ends the account's sessions outright.
    tokens = await service.refresh(body.refresh_token, user_agent=request.headers.get("user-agent"))
    return RefreshResponse(
        access_token=tokens.access_token,
        refresh_token=tokens.refresh_token,
        expires_in=tokens.expires_in,
    )


@router.post(
    "/logout",
    status_code=status.HTTP_204_NO_CONTENT,
    response_class=Response,
    summary="Revoke a refresh token chain",
)
async def logout(
    body: LogoutRequest,
    service: AuthService = Depends(get_auth_service),
) -> Response:
    await service.logout(body.refresh_token)
    return Response(status_code=status.HTTP_204_NO_CONTENT)
