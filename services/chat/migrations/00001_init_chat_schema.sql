-- Chat service schema: the room projection, its membership, the messages, and
-- this service's idempotency ledger.
--
-- Three of the four tables are a *projection*. Rooms and membership are not
-- decided here — the trip service owns who is on a trip, and this schema is fed
-- entirely by `chat.trip-events` (CLAUDE.md rule 1: no join across a service
-- boundary, no synchronous call to trip). `messages` is the only table this
-- service is authoritative for.

-- +goose Up

-- One room per trip, keyed by the trip's own id rather than by a chat id of its
-- own.
--
-- There is no second identifier to keep in step, no lookup between "the trip
-- the user is looking at" and "the room to open", and `trip.created` arriving
-- twice cannot make two rooms. The trip id is the natural key and inventing a
-- surrogate would only add a mapping that can be wrong.
CREATE TABLE rooms (
    trip_id    uuid        PRIMARY KEY,
    title      text        NOT NULL,
    status     text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- Only two values are ever written by this build: `recruiting` when
    -- `trip.created` provisions the room, and `cancelled` when
    -- `trip.cancelled` closes it. The trip service has five lifecycle states,
    -- but chat is bound to exactly four routing keys (see
    -- deploy/rabbitmq/definitions.json) and never hears about the other
    -- transitions — a room is open or it is not. Teaching chat about
    -- `in_progress` means a binding, a handler and a migration, in that order.
    CONSTRAINT rooms_status_known CHECK (status IN ('recruiting', 'cancelled'))
);

-- Who may read and write a room.
--
-- The foreign key is safe only because of how the projection is written, and
-- the two have to be read together.
--
-- Events are delivered at least once and in no particular order
-- (contracts/events.md), so a `join_request.approved` can be handled before the
-- `trip.created` for the same trip — a busy organizer approving within
-- milliseconds of publishing, two relay replicas, or a plain redelivery are all
-- enough. A membership insert against a room that does not exist yet would fail
-- the constraint, retry three times, and land in the DLQ.
--
-- What makes it safe is that every event which adds a member also carries the
-- trip's title, so each handler provisions the room before touching membership
-- (see internal/store/projection.go). The row is therefore always there, and
-- the constraint is the backstop that keeps a future handler from forgetting.
CREATE TABLE room_members (
    trip_id   uuid        NOT NULL REFERENCES rooms (trip_id) ON DELETE CASCADE,
    user_id   uuid        NOT NULL,
    joined_at timestamptz NOT NULL DEFAULT now(),

    -- Set by `participant.removed`; the row is never deleted. The messages this
    -- user wrote stay in the room and stay attributed, and revoking access is a
    -- forward-looking act, not a rewriting of what everyone else already read.
    left_at   timestamptz,

    -- The read watermark behind the unread count on GET /api/chat/rooms, moved
    -- by the socket's `read` frame.
    --
    -- Per-user local state rather than part of the projection: it is the one
    -- column in this table the trip service knows nothing about, and it lives
    -- here because the key it needs — (trip_id, user_id) — is already this
    -- table's primary key. 0 is "has read nothing", which is correct for a new
    -- member because `messages.id` is a bigserial starting at 1.
    last_read_message_id bigint NOT NULL DEFAULT 0,

    PRIMARY KEY (trip_id, user_id),

    CONSTRAINT room_members_read_watermark_nonnegative CHECK (last_read_message_id >= 0)
);

-- The one table this service owns outright.
--
-- `id` is a bigserial rather than a uuid, and it is the only identifier in the
-- system that is not one. A chat room is read backwards in pages of "everything
-- before message N" and the ordering has to be total, monotonic and cheap to
-- compare; a v4 uuid is none of those, and ordering by created_at needs a
-- tiebreaker that a sequence gives for free. The client's `last_message_id` and
-- the `before_id` cursor are both this number.
CREATE TABLE messages (
    id         bigserial   PRIMARY KEY,
    trip_id    uuid        NOT NULL,
    sender_id  uuid        NOT NULL,
    body       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- The length rule is enforced in Go, on the trimmed body, before the insert
    -- (internal/domain). This is the backstop that keeps a hand-written INSERT
    -- from putting an empty bubble in somebody's room.
    CONSTRAINT messages_body_length CHECK (char_length(body) BETWEEN 1 AND 2000)
);

-- Every read path in this service is "the newest N messages in this room,
-- optionally before id X", which is exactly this index read backwards. It also
-- answers the unread count (`id > watermark` within one trip) and the
-- last-message lookup on the room list.
CREATE INDEX messages_trip_id_desc_idx ON messages (trip_id, id DESC);

-- The consumer-side idempotency ledger. Shape fixed by CLAUDE.md rule 5: the
-- same table in every consuming service, in that service's own database. The
-- check and the projection write happen in one transaction, so a redelivery
-- after a crash between them is impossible rather than merely unlikely.
CREATE TABLE processed_events (
    event_id     uuid        PRIMARY KEY,
    processed_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down

DROP TABLE processed_events;
DROP INDEX messages_trip_id_desc_idx;
DROP TABLE messages;
DROP TABLE room_members;
DROP TABLE rooms;
