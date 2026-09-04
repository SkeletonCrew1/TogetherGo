-- Two background workers' worth of schema: the lifecycle scheduler's
-- exactly-once guard, and the local projection of identity's users.
--
-- Neither table crosses a service boundary. `user_ref` is a *copy* of data
-- identity owns, fed by events and never written by a join (CLAUDE.md rule 1);
-- `processed_events` is this service's own idempotency ledger (rule 5).

-- +goose Up

-- The exactly-once guard on `trip.completed`.
--
-- The status check alone is not enough. `in_progress -> completed` being a
-- legal edge and `completed` being terminal means the transition cannot run
-- twice *through the state machine* — but the event is what opens the rating
-- window in identity, and "cannot happen twice" is a claim worth making in the
-- one place that cannot be argued with. A trip whose roster was the organizer
-- alone completes with this column still false: no event was emitted, because
-- there is nobody to rate.
ALTER TABLE trips
    ADD COLUMN completed_event_emitted boolean NOT NULL DEFAULT false;

-- The scheduler's second claim query: in_progress trips whose end_at has
-- passed. Partial, because in a healthy system its answer is nearly always the
-- empty set and the index should be proportional to the trips actually under
-- way rather than to the table.
--
-- The first claim query — recruiting trips whose start_at has passed — needs no
-- index of its own: trips_status_start_idx is (status, start_at, id) and that
-- is exactly its access path. This one has no counterpart, because nothing else
-- in the service has ever wanted to order trips by when they finish.
CREATE INDEX trips_due_to_complete_idx
    ON trips (end_at)
    WHERE status = 'in_progress';

-- The consumer-side idempotency ledger. Shape fixed by CLAUDE.md rule 5: the
-- same table in every consuming service, in that service's own database. The
-- check and the projection write happen in one transaction, so a redelivery
-- after a crash between them is impossible rather than merely unlikely.
CREATE TABLE processed_events (
    event_id     uuid        PRIMARY KEY,
    processed_at timestamptz NOT NULL DEFAULT now()
);

-- The local projection of the users this service displays.
--
-- Read-only in the sense that matters: nothing here is authoritative. Identity
-- owns these fields and publishes user.profile_updated / user.rating_updated;
-- this table is what makes rendering a search page a local read instead of an
-- HTTP call, and what keeps that page rendering names while identity is down.
--
-- `full_name` is nullable on purpose, and a NULL means "this row has not been
-- told a name yet" — which happens when a rating event arrives for a user no
-- profile event has covered. Such a row is *not* a projection hit: see
-- store.UserRefs, which filters them out so the id falls through to identity's
-- batch resolver and comes back filled in.
CREATE TABLE user_ref (
    user_id      uuid          PRIMARY KEY,
    full_name    text,
    photo_url    text,
    rating_avg   numeric(3,2),
    rating_count integer       NOT NULL DEFAULT 0,

    -- Per-source watermarks, and the reason there are two of them.
    --
    -- contracts/events.md requires a consumer to discard an update whose
    -- `updated_at` is older than the one it has stored. Profile updates and
    -- rating updates carry independent timestamps from independent causes, so
    -- a single watermark would let a rating recorded at 12:05 silently reject a
    -- profile edit made at 12:03 and delivered afterwards — a name that stays
    -- wrong until the user edits it again. One watermark per source, each
    -- compared only against its own kind.
    --
    -- NULL means "no event of this kind has been applied", which is also what
    -- lets the identity fallback backfill a field without overwriting anything
    -- an event put there.
    profile_updated_at timestamptz,
    rating_updated_at  timestamptz,

    -- When this row was last written, by any path including the backfill.
    -- Operational, not a watermark: nothing compares against it.
    updated_at   timestamptz   NOT NULL DEFAULT now(),

    CONSTRAINT user_ref_rating_count_nonnegative CHECK (rating_count >= 0),
    CONSTRAINT user_ref_rating_avg_range CHECK (rating_avg IS NULL OR rating_avg BETWEEN 1 AND 5)
);

-- +goose Down

DROP TABLE user_ref;
DROP TABLE processed_events;
DROP INDEX trips_due_to_complete_idx;
ALTER TABLE trips DROP COLUMN completed_event_emitted;
