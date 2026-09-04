package users

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/togethergo/trip/internal/domain"
)

// Projection is the local `user_ref` table, as this package needs it.
//
// An interface rather than *store.Store so that the wiring stays one-way —
// internal/users knows nothing about pgx — and so the tests here can drive the
// cold-start and partial-hit paths without a database.
type Projection interface {
	// UserRefs returns the projection rows for whichever ids have one. A miss
	// is an absent key, never an error.
	UserRefs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]domain.UserRef, error)

	// BackfillUserRefs writes what identity returned into the projection,
	// without overwriting anything an event has already written.
	BackfillUserRefs(ctx context.Context, refs []domain.UserRef) error
}

// ProjectionResolver answers from the local projection first and asks identity
// only about the ids it did not have.
//
// This is a deliberate hybrid, and both halves earn their place:
//
//   - **The projection makes the hot read path local.** A discovery page used
//     to cost one HTTP call to another service; now it costs one indexed SELECT
//     against a table in this service's own database. It also keeps working
//     while identity is down or restarting, which the cache in front of the
//     HTTP client only did for users somebody had happened to look at in the
//     last sixty seconds.
//
//   - **The fallback means a gap degrades rather than breaks.** A projection is
//     only as complete as the events it has consumed. A cold start, a queue
//     purged during an incident, a user created while this service was down —
//     each of those leaves ids the projection has never heard of, and without
//     the fallback the page would render a blank name for them *permanently*,
//     until some unrelated profile edit happened to fill it in. With it, the
//     first page that needs such a user pays one HTTP call, backfills the row,
//     and every page after that is local again. A missed event costs a slower
//     correct answer instead of a wrong one.
//
// Failure of *both* is still not a failure of the page: Resolve returns
// whatever it has together with the error, and every caller in internal/http
// logs it and renders the rest. That rule is older than this type and is not
// weakened by it.
type ProjectionResolver struct {
	projection Projection
	upstream   Resolver
	logger     *slog.Logger
}

// ProjectionOptions configures a ProjectionResolver. Upstream may be nil, which
// is what a service configured without an identity URL gets: the projection
// then answers on its own and a cold id simply has no name.
type ProjectionOptions struct {
	Projection Projection
	Upstream   Resolver
	Logger     *slog.Logger
}

// NewProjectionResolver wraps an upstream resolver with the local-first read.
func NewProjectionResolver(opts ProjectionOptions) *ProjectionResolver {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ProjectionResolver{
		projection: opts.Projection,
		upstream:   opts.Upstream,
		logger:     logger,
	}
}

// Resolve returns what is known about each id, from the projection where
// possible and from identity for the rest.
func (r *ProjectionResolver) Resolve(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]User, error) {
	wanted := dedupe(ids)
	if len(wanted) == 0 {
		return map[uuid.UUID]User{}, nil
	}

	resolved := make(map[uuid.UUID]User, len(wanted))
	missing := wanted

	if r.projection != nil {
		refs, err := r.projection.UserRefs(ctx, wanted)
		if err != nil {
			// The projection lives in this service's own database, so a failure
			// here is a real problem — but it is still not this request's
			// problem to die of. Every id falls through to identity and the
			// page is served, slower.
			r.logger.Warn("user projection read failed, falling back to identity",
				slog.String("error", err.Error()),
				slog.Int("requested", len(wanted)),
			)
		} else {
			missing = make([]uuid.UUID, 0, len(wanted))
			for _, id := range wanted {
				if ref, hit := refs[id]; hit {
					resolved[id] = fromRef(ref)
					continue
				}
				missing = append(missing, id)
			}
		}
	}

	if len(missing) == 0 || r.upstream == nil {
		return resolved, nil
	}

	fetched, err := r.upstream.Resolve(ctx, missing)
	for id, user := range fetched {
		resolved[id] = user
	}
	// Backfilled even on a partial failure: the users that did come back are
	// just as worth keeping as they would have been on a clean call, and the
	// ones that did not are simply absent from the map.
	r.backfill(ctx, fetched)

	return resolved, err
}

// backfill writes what identity returned into the projection.
//
// Best-effort on purpose, and after the response has been assembled: this is a
// cache warm, and a page must not fail — or wait — because a warm did. The
// error is logged and dropped.
func (r *ProjectionResolver) backfill(ctx context.Context, fetched map[uuid.UUID]User) {
	if r.projection == nil || len(fetched) == 0 {
		return
	}

	refs := make([]domain.UserRef, 0, len(fetched))
	for _, user := range fetched {
		refs = append(refs, domain.UserRef{
			UserID:      user.ID,
			FullName:    user.FullName,
			PhotoURL:    user.PhotoURL,
			RatingAvg:   user.RatingAvg,
			RatingCount: user.RatingCount,
		})
	}

	if err := r.projection.BackfillUserRefs(ctx, refs); err != nil {
		r.logger.Warn("user projection backfill failed",
			slog.String("error", err.Error()),
			slog.Int("users", len(refs)),
		)
	}
}

// fromRef converts a projection row into the shape the HTTP layer renders. The
// two are deliberately the same fields, so this is a rename and nothing else.
func fromRef(ref domain.UserRef) User {
	return User{
		ID:          ref.UserID,
		FullName:    ref.FullName,
		PhotoURL:    ref.PhotoURL,
		RatingAvg:   ref.RatingAvg,
		RatingCount: ref.RatingCount,
	}
}
