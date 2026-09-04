package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/events"
)

// This file is the write half of the user projection — the consumer's side —
// and its read half, which the discovery and participation pages go through.
//
// Nothing here is authoritative. Identity owns display names, avatars and
// ratings; `user_ref` is a copy fed by user.profile_updated and
// user.rating_updated, and its only job is to make the hot read path a local
// one. See the README's "The user projection" section for why the fallback to
// identity's batch resolver was kept rather than removed once the projection
// existed.

// markProcessed records that an event has been handled, and reports whether
// this call is the one that recorded it.
//
// `ON CONFLICT DO NOTHING` rather than a SELECT followed by an INSERT: the
// check and the claim are one statement, so two consumers racing on a
// redelivery cannot both decide they are first. A false return means somebody
// already processed this event_id and the caller must not apply the payload
// again — CLAUDE.md rule 5, check-then-process inside one transaction.
func markProcessed(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO processed_events (event_id) VALUES ($1)
		ON CONFLICT (event_id) DO NOTHING`, eventID)
	if err != nil {
		return false, fmt.Errorf("mark event %s processed: %w", eventID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkProcessed records an event that needs no projection write — an
// event_type this build does not recognise, which contracts/events.md says to
// log, mark processed and ack rather than to dead-letter.
func (s *Store) MarkProcessed(ctx context.Context, eventID uuid.UUID) (bool, error) {
	var first bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, err = markProcessed(ctx, tx, eventID)
		return err
	})
	return first, err
}

// ApplyProfileUpdate projects user.profile_updated onto user_ref.
//
// Returns false when the event had already been processed, which is the
// consumer's cue to ack without acting. The idempotency check and the upsert
// are one transaction, so a crash between them is not a state this service can
// be in.
func (s *Store) ApplyProfileUpdate(ctx context.Context, eventID uuid.UUID, p events.UserProfileUpdated) (bool, error) {
	var first bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, err = markProcessed(ctx, tx, eventID)
		if err != nil || !first {
			return err
		}

		// The WHERE on the DO UPDATE is the out-of-order rule from
		// contracts/events.md: an update older than the one already applied is
		// discarded, not written. It is still marked processed — a message that
		// was correctly ignored has been handled.
		//
		// `<=` and not `<` so that a replay of the newest event is a harmless
		// rewrite of identical data rather than a silent no-op that would make
		// the two branches behave differently for no reason.
		_, err = tx.Exec(ctx, `
			INSERT INTO user_ref (user_id, full_name, photo_url, profile_updated_at, updated_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (user_id) DO UPDATE SET
				full_name          = EXCLUDED.full_name,
				photo_url          = EXCLUDED.photo_url,
				profile_updated_at = EXCLUDED.profile_updated_at,
				updated_at         = now()
			WHERE user_ref.profile_updated_at IS NULL
			   OR user_ref.profile_updated_at <= EXCLUDED.profile_updated_at`,
			p.UserID, p.DisplayName, p.AvatarURL, p.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("project profile update for %s: %w", p.UserID, err)
		}
		return nil
	})
	return first, err
}

// ApplyRatingUpdate projects user.rating_updated onto user_ref.
//
// A rating for a user no profile event has covered creates a row with a NULL
// full_name. That is deliberate and it is not a hit: UserRefs filters those
// rows out, so the id falls through to identity's batch resolver and comes back
// with a name, which the backfill then writes. Storing a rating for a user
// whose name is unknown is better than dropping it — the name is one HTTP call
// away, and the rating would otherwise be lost until the next rating event.
func (s *Store) ApplyRatingUpdate(ctx context.Context, eventID uuid.UUID, p events.UserRatingUpdated) (bool, error) {
	var first bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, err = markProcessed(ctx, tx, eventID)
		if err != nil || !first {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO user_ref (user_id, rating_avg, rating_count, rating_updated_at, updated_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (user_id) DO UPDATE SET
				rating_avg        = EXCLUDED.rating_avg,
				rating_count      = EXCLUDED.rating_count,
				rating_updated_at = EXCLUDED.rating_updated_at,
				updated_at        = now()
			WHERE user_ref.rating_updated_at IS NULL
			   OR user_ref.rating_updated_at <= EXCLUDED.rating_updated_at`,
			p.UserID, p.RatingAverage, p.RatingCount, p.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("project rating update for %s: %w", p.UserID, err)
		}
		return nil
	})
	return first, err
}

// UserRefs reads the projection for a page's worth of ids.
//
// Rows with a NULL full_name are excluded, which makes this method answer the
// question the caller is actually asking — "which of these can I render?" —
// rather than "which of these have a row?". A rating-only row has a row and
// nothing to show.
//
// A miss is not an error and produces no entry: the caller falls back to
// identity for whatever is absent.
func (s *Store) UserRefs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]domain.UserRef, error) {
	refs := make(map[uuid.UUID]domain.UserRef, len(ids))
	if len(ids) == 0 {
		return refs, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT user_id, full_name, photo_url, rating_avg, rating_count
		FROM user_ref
		WHERE user_id = ANY($1) AND full_name IS NOT NULL`, ids)
	if err != nil {
		return nil, fmt.Errorf("select user_ref: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ref domain.UserRef
		if err := rows.Scan(&ref.UserID, &ref.FullName, &ref.PhotoURL, &ref.RatingAvg, &ref.RatingCount); err != nil {
			return nil, fmt.Errorf("scan user_ref: %w", err)
		}
		refs[ref.UserID] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user_ref: %w", err)
	}
	return refs, nil
}

// BackfillUserRefs writes what identity's batch resolver returned into the
// projection, without ever overwriting what an event put there.
//
// The CASE expressions are that rule. A field group whose watermark is NULL has
// never been told anything by an event and is filled in; one that has is left
// exactly as it is. Identity's REST snapshot and its event stream are the same
// data from the same owner, but only the event stream carries a timestamp this
// service can order against, so the stream wins by default and the snapshot
// fills the gaps.
//
// The whole batch is one statement, zipped with unnest, so warming a page's
// worth of users costs one round trip.
func (s *Store) BackfillUserRefs(ctx context.Context, refs []domain.UserRef) error {
	if len(refs) == 0 {
		return nil
	}

	ids := make([]uuid.UUID, len(refs))
	names := make([]string, len(refs))
	photos := make([]*string, len(refs))
	averages := make([]*float64, len(refs))
	counts := make([]int32, len(refs))

	for i, ref := range refs {
		ids[i] = ref.UserID
		names[i] = ref.FullName
		photos[i] = ref.PhotoURL
		averages[i] = ref.RatingAvg
		counts[i] = int32(ref.RatingCount)
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_ref (user_id, full_name, photo_url, rating_avg, rating_count, updated_at)
		SELECT u.user_id, u.full_name, u.photo_url, u.rating_avg, u.rating_count, now()
		FROM unnest($1::uuid[], $2::text[], $3::text[], $4::numeric[], $5::int[])
		     AS u(user_id, full_name, photo_url, rating_avg, rating_count)
		ON CONFLICT (user_id) DO UPDATE SET
			full_name = CASE WHEN user_ref.profile_updated_at IS NULL
			                 THEN EXCLUDED.full_name ELSE user_ref.full_name END,
			photo_url = CASE WHEN user_ref.profile_updated_at IS NULL
			                 THEN EXCLUDED.photo_url ELSE user_ref.photo_url END,
			rating_avg = CASE WHEN user_ref.rating_updated_at IS NULL
			                  THEN EXCLUDED.rating_avg ELSE user_ref.rating_avg END,
			rating_count = CASE WHEN user_ref.rating_updated_at IS NULL
			                    THEN EXCLUDED.rating_count ELSE user_ref.rating_count END,
			updated_at = now()`,
		ids, names, photos, averages, counts,
	); err != nil {
		return fmt.Errorf("backfill user_ref: %w", err)
	}
	return nil
}
