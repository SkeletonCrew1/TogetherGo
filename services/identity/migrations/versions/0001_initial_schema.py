"""initial identity schema: users, refresh_tokens, outbox

Revision ID: 0001
Revises:
Create Date: 2026-08-27

"""

from __future__ import annotations

from collections.abc import Sequence

import sqlalchemy as sa
from alembic import op
from sqlalchemy.dialects import postgresql

revision: str = "0001"
down_revision: str | None = None
branch_labels: str | Sequence[str] | None = None
depends_on: str | Sequence[str] | None = None


def upgrade() -> None:
    # citext is a trusted extension, so the database owner can install it
    # without superuser. It makes the unique index on users.email
    # case-insensitive, which is the behaviour every caller already assumes.
    op.execute("CREATE EXTENSION IF NOT EXISTS citext")

    op.create_table(
        "users",
        sa.Column("id", postgresql.UUID(as_uuid=True), primary_key=True),
        sa.Column("email", postgresql.CITEXT(), nullable=False),
        sa.Column("password_hash", sa.Text(), nullable=False),
        sa.Column("full_name", sa.Text(), nullable=False),
        sa.Column("bio", sa.Text(), nullable=True),
        sa.Column("birth_date", sa.Date(), nullable=True),
        sa.Column("phone", sa.Text(), nullable=True),
        sa.Column("photo_url", sa.Text(), nullable=True),
        sa.Column("is_active", sa.Boolean(), nullable=False, server_default=sa.text("true")),
        sa.Column(
            "created_at",
            sa.DateTime(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        sa.Column(
            "updated_at",
            sa.DateTime(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        sa.UniqueConstraint("email", name="users_email_key"),
    )

    op.create_table(
        "refresh_tokens",
        sa.Column("id", postgresql.UUID(as_uuid=True), primary_key=True),
        sa.Column("user_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("token_hash", postgresql.BYTEA(), nullable=False),
        sa.Column(
            "issued_at",
            sa.DateTime(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        sa.Column("expires_at", sa.DateTime(timezone=True), nullable=False),
        sa.Column("revoked_at", sa.DateTime(timezone=True), nullable=True),
        sa.Column("replaced_by", postgresql.UUID(as_uuid=True), nullable=True),
        sa.Column("user_agent", sa.Text(), nullable=True),
        sa.ForeignKeyConstraint(
            ["user_id"], ["users.id"], name="refresh_tokens_user_id_fkey", ondelete="CASCADE"
        ),
        # Self-reference: the successor a rotation replaced this row with.
        # SET NULL rather than CASCADE — losing the pointer is survivable,
        # deleting the predecessor of a pruned row is not what we mean.
        sa.ForeignKeyConstraint(
            ["replaced_by"],
            ["refresh_tokens.id"],
            name="refresh_tokens_replaced_by_fkey",
            ondelete="SET NULL",
        ),
        sa.UniqueConstraint("token_hash", name="refresh_tokens_token_hash_key"),
    )
    # Serves both hot paths: "every live token for this user" during reuse
    # detection, and expiry sweeps.
    op.create_index(
        "refresh_tokens_user_id_expires_at_idx",
        "refresh_tokens",
        ["user_id", "expires_at"],
    )

    op.create_table(
        "outbox",
        sa.Column("id", postgresql.UUID(as_uuid=True), primary_key=True),
        sa.Column("aggregate_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("event_type", sa.Text(), nullable=False),
        sa.Column("payload", postgresql.JSONB(), nullable=False),
        sa.Column(
            "created_at",
            sa.DateTime(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        sa.Column("published_at", sa.DateTime(timezone=True), nullable=True),
    )
    # The relay only ever reads unpublished rows; a partial index keeps that
    # scan proportional to the backlog rather than to the table.
    op.create_index(
        "outbox_unpublished_idx",
        "outbox",
        ["created_at"],
        postgresql_where=sa.text("published_at IS NULL"),
    )


def downgrade() -> None:
    op.drop_index("outbox_unpublished_idx", table_name="outbox")
    op.drop_table("outbox")
    op.drop_index("refresh_tokens_user_id_expires_at_idx", table_name="refresh_tokens")
    op.drop_table("refresh_tokens")
    op.drop_table("users")
    # citext is left installed: dropping it would fail against any other
    # object that came to depend on it, and an unused extension costs nothing.
