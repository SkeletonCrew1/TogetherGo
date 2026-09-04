-- Trip service schema: trips, their ordered route points, their participants,
-- and the transactional outbox every publisher in this system writes to.
--
-- PostGIS is not created here. `deploy/postgres/init/01-databases.sql` installs
-- the extension into trip_db as the superuser, because CREATE EXTENSION needs
-- privileges trip_user does not have and should not be given.

-- +goose Up

CREATE TABLE trips (
    id              uuid        PRIMARY KEY,
    organizer_id    uuid        NOT NULL,
    title           text        NOT NULL,
    description     text,
    category        text        NOT NULL,
    status          text        NOT NULL,
    capacity        integer     NOT NULL,
    approved_count  integer     NOT NULL DEFAULT 1,
    start_at        timestamptz NOT NULL,
    end_at          timestamptz NOT NULL,

    -- Denormalised copies of the first and last trip_point's location.
    --
    -- They are duplicated on purpose. The search endpoint filters trips by
    -- "starts within N km of me", and with the coordinates only in trip_points
    -- that predicate becomes a correlated subquery or a join against a table
    -- with ~20 rows per trip — neither of which a GIST index on trip_points can
    -- serve as the driving access path for a scan over `trips`. Held here, the
    -- filter is `ST_DWithin(departure_location, $1, $2)` directly on the table
    -- being scanned, and trips_departure_gist answers it.
    --
    -- The duplication is only safe because it is never written on its own:
    -- every statement that writes trip_points rewrites these two columns in the
    -- same transaction (see store.writePoints). There is no code path that
    -- updates one without the other.
    departure_location   geography(Point, 4326) NOT NULL,
    destination_location geography(Point, 4326) NOT NULL,

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT trips_capacity_range   CHECK (capacity BETWEEN 2 AND 50),
    CONSTRAINT trips_dates_ordered    CHECK (end_at >= start_at),
    -- The status vocabulary is enforced in Go by the transition map in
    -- internal/domain/status.go. This constraint is the backstop that keeps a
    -- typo or a hand-written UPDATE from inventing a sixth state.
    CONSTRAINT trips_status_known     CHECK (status IN ('draft', 'recruiting', 'in_progress', 'completed', 'cancelled')),
    -- The organizer always occupies a seat, so approved_count starts at 1 and
    -- can never exceed capacity. Join-request approval (a later stage) relies
    -- on this to make overbooking impossible rather than merely unlikely.
    CONSTRAINT trips_approved_fits    CHECK (approved_count >= 1 AND approved_count <= capacity)
);

CREATE TABLE trip_points (
    id        uuid                   PRIMARY KEY,
    trip_id   uuid                   NOT NULL REFERENCES trips (id) ON DELETE CASCADE,
    seq       integer                NOT NULL,
    name      text                   NOT NULL,
    location  geography(Point, 4326) NOT NULL,
    arrive_at timestamptz,
    transport text,

    CONSTRAINT trip_points_seq_nonnegative CHECK (seq >= 0),
    UNIQUE (trip_id, seq)
);

CREATE TABLE participants (
    trip_id   uuid        NOT NULL REFERENCES trips (id) ON DELETE CASCADE,
    user_id   uuid        NOT NULL,
    role      text        NOT NULL,
    joined_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (trip_id, user_id),
    CONSTRAINT participants_role_known CHECK (role IN ('organizer', 'participant'))
);

-- Radius filters on the search endpoint.
CREATE INDEX trips_departure_gist   ON trips USING GIST (departure_location);
CREATE INDEX trips_destination_gist ON trips USING GIST (destination_location);

-- The browse ordering: recruiting trips, soonest first. `id` is the tiebreaker
-- that makes the keyset cursor total, so it belongs in the index.
CREATE INDEX trips_status_start_idx ON trips (status, start_at, id);

-- "My trips", filtered by status.
CREATE INDEX trips_organizer_status_idx ON trips (organizer_id, status);

-- Transactional outbox. Shape fixed by contracts/events.md — the same table in
-- every publishing service, in that service's own database.
CREATE TABLE outbox (
    id           uuid        PRIMARY KEY,
    aggregate_id uuid        NOT NULL,
    event_type   text        NOT NULL,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);

-- The relay only ever reads unpublished rows. A partial index keeps that scan
-- proportional to the backlog rather than to the table.
CREATE INDEX outbox_unpublished_idx
    ON outbox (created_at)
    WHERE published_at IS NULL;

-- +goose Down

DROP TABLE outbox;
DROP TABLE participants;
DROP TABLE trip_points;
DROP TABLE trips;
