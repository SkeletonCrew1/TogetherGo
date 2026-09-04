"""profile aggregates and the access-token link on refresh tokens

Two additions, both driven by endpoints in this revision:

* `users.rating_avg` / `users.rating_count` — the aggregate a public profile
  shows and `user.rating_updated` publishes. They are denormalised onto the
  user row on purpose: identity owns ratings (`/api/ratings` routes here), and
  every profile read wants the aggregate while almost none want the individual
  ratings behind it.
* `refresh_tokens.access_token_jti` — which access token was minted alongside
  this refresh token. `POST /api/users/me/password` has to revoke every
  session *except the caller's own*, and an access token carries no other
  handle on the session that issued it.

Revision ID: 0002
Revises: 0001
Create Date: 2026-08-28

"""

from __future__ import annotations

from collections.abc import Sequence

import sqlalchemy as sa
from alembic import op
from sqlalchemy.dialects import postgresql

revision: str = "0002"
down_revision: str | None = "0001"
branch_labels: str | Sequence[str] | None = None
depends_on: str | Sequence[str] | None = None


def upgrade() -> None:
    op.add_column(
        "users",
        # NULL, not 0: "no ratings yet" and "rated zero" are different facts,
        # and a new account showing 0.00 out of 5 would be a lie about it.
        # numeric(3,2) is exactly the 1.00–5.00 with two decimals that
        # contracts/events.md pins `rating_average` to.
        sa.Column("rating_avg", sa.Numeric(3, 2), nullable=True),
    )
    op.add_column(
        "users",
        sa.Column(
            "rating_count", sa.Integer(), nullable=False, server_default=sa.text("0")
        ),
    )
    op.create_check_constraint(
        "users_rating_avg_range",
        "users",
        "rating_avg IS NULL OR (rating_avg >= 1.00 AND rating_avg <= 5.00)",
    )
    op.create_check_constraint(
        "users_rating_count_non_negative",
        "users",
        "rating_count >= 0",
    )
    # The two columns move together — an average with no ratings behind it, or
    # a count with no average, is a bug in whatever wrote it.
    op.create_check_constraint(
        "users_rating_avg_matches_count",
        "users",
        "(rating_avg IS NULL) = (rating_count = 0)",
    )

    op.add_column(
        "refresh_tokens",
        sa.Column("access_token_jti", postgresql.UUID(as_uuid=True), nullable=True),
    )
    # Nullable because a row can outlive the reason to know: nothing reads it
    # except the "revoke every session but mine" lookup, which tolerates a
    # miss by revoking more rather than less.
    op.create_index(
        "refresh_tokens_access_token_jti_idx",
        "refresh_tokens",
        ["access_token_jti"],
        unique=True,
        postgresql_where=sa.text("access_token_jti IS NOT NULL"),
    )


def downgrade() -> None:
    op.drop_index("refresh_tokens_access_token_jti_idx", table_name="refresh_tokens")
    op.drop_column("refresh_tokens", "access_token_jti")
    op.drop_constraint("users_rating_avg_matches_count", "users", type_="check")
    op.drop_constraint("users_rating_count_non_negative", "users", type_="check")
    op.drop_constraint("users_rating_avg_range", "users", type_="check")
    op.drop_column("users", "rating_count")
    op.drop_column("users", "rating_avg")
