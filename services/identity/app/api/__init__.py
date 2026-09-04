from app.api.auth import router as auth_router
from app.api.health import router as health_router
from app.api.internal import router as internal_router
from app.api.jwks import router as jwks_router
from app.api.ratings import router as ratings_router
from app.api.ratings import user_ratings_router
from app.api.users import router as users_router

__all__ = [
    "auth_router",
    "health_router",
    "internal_router",
    "jwks_router",
    "ratings_router",
    "user_ratings_router",
    "users_router",
]
