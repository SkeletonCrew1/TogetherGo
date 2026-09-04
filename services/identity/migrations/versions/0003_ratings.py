"""ratings, the pending rating window, the per-user aggregate, processed_events

Ratings live in identity and not in trip: a rating is a property of a *user*,
and `GET /api/users/{id}` has to be able to show the aggregate without a call
into another service. `trip.completed` carries the full roster precisely so
that this service can open the rating window without asking trip who
travelled (contracts/events.md).

The aggregate moves from two columns on `users` to its own table:

* `users.rating_avg` / `users.rating_count` were denormalised onto the row in
  0002, before anything wrote them. Keeping a *stored average* is the wrong
  shape once there is a writer: an average cannot be incremented, so every
  submission would have to recompute it from a number it does not have. The
  sum can be incremented, and the average is a division — so `user_rating`
  stores `rating_sum` and `rating_count` and the API divides on the way out.
* Splitting it off also keeps the write path off the `users` row. Rating
  someone should not contend with that person editing their own profile.

Revision ID: 0003
Revises: 0002
Create Date: 2026-08-31

"""

from __future__ import annotations

from collections.abc import Sequence

import sqlalchemy as sa
from alembic import op
from sqlalchemy.dialects import postgresql

revision: str = "0003"
down_revision: str | None = "0002"
branch_labels: str | Sequence[str] | None = None
depends_on: str | Sequence[str] | None = None


def upgrade() -> None:
    # --- the aggregate ------------------------------------------------------
    op.create_table(
        "user_rating",
        # No FK to users. The row is created by the rating submission for
        # whoever was rated, and a user who later deletes their account should
        # not take the counter with them silently — nothing here reads a rating
        # for an id that has no user row anyway.
        sa.Column("user_id", postgresql.UUID(as_uuid=True), primary_key=True),
        # bigint because it only ever grows: 5 per rating, and nothing ever
        # subtracts. int would be a ceiling somebody eventually finds.
        sa.Column("rating_sum", sa.BigInteger(), nullable=False, server_default=sa.text("0")),
        sa.Column("rating_count", sa.Integer(), nullable=False, server_default=sa.text("0")),
        sa.CheckConstraint("rating_count >= 0", name="user_rating_count_non_negative"),
        sa.CheckConstraint("rating_sum >= 0", name="user_rating_sum_non_negative"),
        # 1..5 per rating, so the sum can never be outside that band. A row
        # that violates this is an increment applied without its count.
        sa.CheckConstraint(
            "rating_sum BETWEEN rating_count AND rating_count * 5",
            name="user_rating_sum_matches_count",
        ),
    )

    # Carry over whatever 0002's columns hold before dropping them. In practice
    # this moves nothing — no code path ever wrote them — but a migration that
    # silently discards rows is a habit worth not forming. round() because the
    # stored average was numeric(3,2) and the sum behind it was integral.
    op.execute(
        """
        INSERT INTO user_rating (user_id, rating_sum, rating_count)
        SELECT id, round(rating_avg * rating_count)::bigint, rating_count
        FROM users
        WHERE rating_count > 0 AND rating_avg IS NOT NULL
        """
    )

    op.drop_constraint("users_rating_avg_matches_count", "users", type_="check")
    op.drop_constraint("users_rating_count_non_negative", "users", type_="check")
    op.drop_constraint("users_rating_avg_range", "users", type_="check")
    op.drop_column("users", "rating_count")
    op.drop_column("users", "rating_avg")

    # --- the ratings themselves ---------------------------------------------
    op.create_table(
        "ratings",
        sa.Column("id", postgresql.UUID(as_uuid=True), primary_key=True),
        sa.Column("trip_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("rater_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("ratee_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("score", sa.SmallInteger(), nullable=False),
        sa.Column("comment", sa.Text(), nullable=True),
        sa.Column(
            "created_at",
            sa.DateTime(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        sa.CheckConstraint("score BETWEEN 1 AND 5", name="ratings_score_range"),
        sa.CheckConstraint("rater_id <> ratee_id", name="ratings_no_self_rating"),
        # One score per (trip, rater, ratee), enforced by the database rather
        # than by a SELECT before the INSERT: two concurrent submissions both
        # pass that SELECT, and only a unique index stops the second one.
        # It is also what makes a rating immutable — there is no UPDATE path.
        sa.UniqueConstraint("trip_id", "rater_id", "ratee_id", name="ratings_one_per_pair"),
    )
    # `GET /api/users/{id}/ratings` reads a ratee's ratings newest first, keyset
    # paginated on (created_at, id). The index carries the tiebreaker so the
    # cursor comparison stays an index qual instead of a filter.
    op.create_index(
        "ratings_ratee_created_idx",
        "ratings",
        ["ratee_id", sa.text("created_at DESC"), sa.text("id DESC")],
    )

    # --- the rating window --------------------------------------------------
    op.create_table(
        "pending_ratings",
        sa.Column("id", postgresql.UUID(as_uuid=True), primary_key=True),
        sa.Column("trip_id", postgresql.UUID(as_uuid=True), nullable=False),
        # Denormalised from the `trip.completed` payload. The pending list and
        # every rating rendered back to a ratee show the trip's title, and
        # identity must not join — or call — trip to get it. A trip renamed
        # after it completed keeps the title it had when it ended, which is the
        # one everyone on it remembers.
        sa.Column("trip_title", sa.Text(), nullable=False),
        sa.Column("rater_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column("ratee_id", postgresql.UUID(as_uuid=True), nullable=False),
        sa.Column(
            "created_at",
            sa.DateTime(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
        sa.Column("expires_at", sa.DateTime(timezone=True), nullable=False),
        # NULL means outstanding. Set when the rating is submitted, and also by
        # the nightly sweep for a window that closed unused — so "completed"
        # here means "no longer actionable", not "rated".
        sa.Column("completed_at", sa.DateTime(timezone=True), nullable=True),
        sa.CheckConstraint("rater_id <> ratee_id", name="pending_ratings_no_self_rating"),
        # The eligibility lookup is exactly this key, so the constraint's index
        # serves the read as well as the ON CONFLICT DO NOTHING of the consumer.
        sa.UniqueConstraint(
            "trip_id", "rater_id", "ratee_id", name="pending_ratings_one_per_pair"
        ),
    )
    # `GET /api/ratings/pending` — one caller's outstanding rows. Partial, so
    # the index holds only what the endpoint reads and shrinks as windows close.
    op.create_index(
        "pending_ratings_outstanding_idx",
        "pending_ratings",
        ["rater_id", "expires_at", "trip_id"],
        postgresql_where=sa.text("completed_at IS NULL"),
    )
    # The nightly sweep's WHERE clause, for the same reason.
    op.create_index(
        "pending_ratings_expiry_idx",
        "pending_ratings",
        ["expires_at"],
        postgresql_where=sa.text("completed_at IS NULL"),
    )

    # --- consumer idempotency ----------------------------------------------
    # CLAUDE.md rule 5. Identity is a consumer as of this revision
    # (`identity.trip-events`), and every consumer in the platform absorbs
    # duplicates with a table in its own database, keyed on the envelope's
    # event_id. The check and the work are one transaction.
    op.create_table(
        "processed_events",
        sa.Column("event_id", postgresql.UUID(as_uuid=True), primary_key=True),
        sa.Column(
            "processed_at",
            sa.DateTime(timezone=True),
            nullable=False,
            server_default=sa.text("now()"),
        ),
    )


def downgrade() -> None:
    op.drop_table("processed_events")
    op.drop_index("pending_ratings_expiry_idx", table_name="pending_ratings")
    op.drop_index("pending_ratings_outstanding_idx", table_name="pending_ratings")
    op.drop_table("pending_ratings")
    op.drop_index("ratings_ratee_created_idx", table_name="ratings")
    op.drop_table("ratings")

    op.add_column("users", sa.Column("rating_avg", sa.Numeric(3, 2), nullable=True))
    op.add_column(
        "users",
        sa.Column("rating_count", sa.Integer(), nullable=False, server_default=sa.text("0")),
    )
    op.execute(
        """
        UPDATE users
        SET rating_count = agg.rating_count,
            rating_avg = round(agg.rating_sum::numeric / agg.rating_count, 2)
        FROM user_rating AS agg
        WHERE agg.user_id = users.id AND agg.rating_count > 0
        """
    )
    op.create_check_constraint(
        "users_rating_avg_range",
        "users",
        "rating_avg IS NULL OR (rating_avg >= 1.00 AND rating_avg <= 5.00)",
    )
    op.create_check_constraint(
        "users_rating_count_non_negative", "users", "rating_count >= 0"
    )
    op.create_check_constraint(
        "users_rating_avg_matches_count",
        "users",
        "(rating_avg IS NULL) = (rating_count = 0)",
    )
    op.drop_table("user_rating")
