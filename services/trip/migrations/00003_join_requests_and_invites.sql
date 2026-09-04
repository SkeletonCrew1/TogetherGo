-- The participation flow: asking to join a trip, and being invited to one.
--
-- Neither table carries a foreign key to a user. Users belong to identity and
-- there is no join across that boundary (CLAUDE.md rule 1) — `user_id` here is
-- a reference to a row in another service's database, and the only thing this
-- service can say about it is that it is a uuid.

-- +goose Up

CREATE TABLE join_requests (
    id         uuid        PRIMARY KEY,
    trip_id    uuid        NOT NULL REFERENCES trips (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL,
    status     text        NOT NULL,
    message    text,

    -- The organizer's free-text reason for a rejection, and the only place it
    -- lives. It is deliberately *not* on the bus: `join_request.rejected`
    -- carries a reason code (`declined_by_organizer`), and the sentence the
    -- organizer typed is visible through this service's API to the requester
    -- alone. See contracts/events.md, join_request.rejected.
    decision_reason text,

    created_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz,
    decided_by uuid,

    CONSTRAINT join_requests_status_known CHECK (
        status IN ('pending', 'approved', 'rejected', 'cancelled')
    ),
    -- A decided request has a decision time and a decider; a pending one has
    -- neither. Written as an equivalence rather than two implications so that
    -- both halves are one constraint and neither can be added without the
    -- other. `decided_by` is the organizer for an approval or a rejection, and
    -- the requester themselves for a cancellation.
    CONSTRAINT join_requests_decision_complete CHECK (
        (status = 'pending') = (decided_at IS NULL AND decided_by IS NULL)
    )
);

-- One open request per user per trip, and no more than that.
--
-- Partial, on `status = 'pending'`: a rejection is not the end of the story —
-- an organizer may reject someone in March and take them in June, and the
-- history of that is worth keeping. A plain UNIQUE (trip_id, user_id) would
-- make a second application impossible; this one makes only a *second open*
-- application impossible, which is the actual rule.
--
-- It is also the backstop under the "already has a pending request" check in
-- Store.CreateJoinRequest: two simultaneous requests from the same user race
-- past a SELECT but not past a unique index.
CREATE UNIQUE INDEX join_requests_one_pending_idx
    ON join_requests (trip_id, user_id)
    WHERE status = 'pending';

-- The organizer's queue: every request on one trip, pending ones first. The
-- leading (trip_id, status) is what that ORDER BY reads.
CREATE INDEX join_requests_trip_idx ON join_requests (trip_id, status, created_at DESC, id DESC);

-- "Have I asked to join this trip?", and the /api/my/requests page a later
-- stage adds.
CREATE INDEX join_requests_user_idx ON join_requests (user_id, created_at DESC);

CREATE TABLE trip_invites (
    id              uuid        PRIMARY KEY,
    trip_id         uuid        NOT NULL REFERENCES trips (id) ON DELETE CASCADE,
    invited_user_id uuid        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),

    -- Not partial, unlike the join-request index above: an invitation is not a
    -- request that can be re-made. The organizer invited this person to this
    -- trip, once, and inviting them again is a mistake worth reporting rather
    -- than a second row.
    UNIQUE (trip_id, invited_user_id)
);

CREATE INDEX trip_invites_user_idx ON trip_invites (invited_user_id, created_at DESC);

-- +goose Down

DROP TABLE trip_invites;
DROP TABLE join_requests;
