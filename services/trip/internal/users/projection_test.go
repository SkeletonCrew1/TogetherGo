package users_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/users"
)

// The projection-first resolver. What is under test is the hybrid itself: which
// source answers, what happens when one of them cannot, and that a fallback
// leaves the projection better off than it found it.

// fakeProjection is user_ref without a database. It records what it was asked
// for and what was backfilled into it, because "one local read, then identity
// only for the rest" is the property that matters and it is invisible from the
// return value alone.
type fakeProjection struct {
	rows map[uuid.UUID]domain.UserRef

	reads     int
	askedFor  [][]uuid.UUID
	backfills [][]domain.UserRef

	readErr     error
	backfillErr error
}

func newFakeProjection() *fakeProjection {
	return &fakeProjection{rows: map[uuid.UUID]domain.UserRef{}}
}

func (p *fakeProjection) UserRefs(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]domain.UserRef, error) {
	p.reads++
	p.askedFor = append(p.askedFor, ids)
	if p.readErr != nil {
		return nil, p.readErr
	}
	found := map[uuid.UUID]domain.UserRef{}
	for _, id := range ids {
		if ref, ok := p.rows[id]; ok {
			found[id] = ref
		}
	}
	return found, nil
}

func (p *fakeProjection) BackfillUserRefs(_ context.Context, refs []domain.UserRef) error {
	p.backfills = append(p.backfills, refs)
	if p.backfillErr != nil {
		return p.backfillErr
	}
	for _, ref := range refs {
		p.rows[ref.UserID] = ref
	}
	return nil
}

// fakeUpstream is identity's batch resolver, counted.
type fakeUpstream struct {
	users    map[uuid.UUID]users.User
	calls    int
	askedFor [][]uuid.UUID
	err      error
}

func (u *fakeUpstream) Resolve(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]users.User, error) {
	u.calls++
	u.askedFor = append(u.askedFor, ids)
	found := map[uuid.UUID]users.User{}
	for _, id := range ids {
		if user, ok := u.users[id]; ok {
			found[id] = user
		}
	}
	return found, u.err
}

func ptr[T any](v T) *T { return &v }

func quiet() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func TestProjectionAnswersWithoutTouchingIdentity(t *testing.T) {
	projection := newFakeProjection()
	upstream := &fakeUpstream{users: map[uuid.UUID]users.User{}}

	alex := uuid.New()
	projection.rows[alex] = domain.UserRef{
		UserID: alex, FullName: "Alex K.", PhotoURL: ptr("https://cdn.example.test/a.jpg"),
		RatingAvg: ptr(4.75), RatingCount: 12,
	}

	resolver := users.NewProjectionResolver(users.ProjectionOptions{
		Projection: projection, Upstream: upstream, Logger: quiet(),
	})

	resolved, err := resolver.Resolve(context.Background(), []uuid.UUID{alex})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	require.Equal(t, "Alex K.", resolved[alex].FullName)
	require.Equal(t, 12, resolved[alex].RatingCount)

	// The whole point: a warm projection means the hot read path never leaves
	// this service.
	require.Zero(t, upstream.calls)
}

func TestOnlyTheMissingIdsGoToIdentityAndAreBackfilled(t *testing.T) {
	projection := newFakeProjection()
	known, cold := uuid.New(), uuid.New()

	projection.rows[known] = domain.UserRef{UserID: known, FullName: "Alex K.", RatingCount: 12}
	upstream := &fakeUpstream{users: map[uuid.UUID]users.User{
		cold: {ID: cold, FullName: "Bohdan", PhotoURL: ptr("https://cdn.example.test/b.jpg"), RatingAvg: ptr(4.9), RatingCount: 20},
	}}

	resolver := users.NewProjectionResolver(users.ProjectionOptions{
		Projection: projection, Upstream: upstream, Logger: quiet(),
	})

	resolved, err := resolver.Resolve(context.Background(), []uuid.UUID{known, cold})
	require.NoError(t, err)
	require.Len(t, resolved, 2)
	require.Equal(t, "Alex K.", resolved[known].FullName)
	require.Equal(t, "Bohdan", resolved[cold].FullName)

	require.Equal(t, 1, upstream.calls)
	require.Equal(t, [][]uuid.UUID{{cold}}, upstream.askedFor,
		"identity is asked only about what the projection did not have")

	// Backfilled, so the next page is local for this user too — the whole
	// reason the fallback is a fallback and not a permanent second source.
	require.Len(t, projection.backfills, 1)
	require.Len(t, projection.backfills[0], 1)
	require.Equal(t, cold, projection.backfills[0][0].UserID)

	second, err := resolver.Resolve(context.Background(), []uuid.UUID{known, cold})
	require.NoError(t, err)
	require.Len(t, second, 2)
	require.Equal(t, 1, upstream.calls, "the backfilled user is a local read now")
}

// TestAColdProjectionStillAnswers is the point of keeping the fallback: a
// service that has consumed no events yet, or lost its queue during an
// incident, degrades to a slower correct answer rather than to blank names.
func TestAColdProjectionStillAnswers(t *testing.T) {
	projection := newFakeProjection()
	alex, bohdan := uuid.New(), uuid.New()
	upstream := &fakeUpstream{users: map[uuid.UUID]users.User{
		alex:   {ID: alex, FullName: "Alex K.", RatingCount: 12},
		bohdan: {ID: bohdan, FullName: "Bohdan", RatingCount: 20},
	}}

	resolver := users.NewProjectionResolver(users.ProjectionOptions{
		Projection: projection, Upstream: upstream, Logger: quiet(),
	})

	resolved, err := resolver.Resolve(context.Background(), []uuid.UUID{alex, bohdan})
	require.NoError(t, err)
	require.Len(t, resolved, 2)
	require.Equal(t, 1, upstream.calls, "one batch call, not one per id")
}

// TestIdentityBeingDownStillServesTheProjection is the other half: the local
// copy is what keeps names on the page while identity is restarting, which is
// more than the sixty-second cache in front of it ever managed.
func TestIdentityBeingDownStillServesTheProjection(t *testing.T) {
	projection := newFakeProjection()
	known, cold := uuid.New(), uuid.New()
	projection.rows[known] = domain.UserRef{UserID: known, FullName: "Alex K.", RatingCount: 12}

	upstream := &fakeUpstream{users: map[uuid.UUID]users.User{}, err: users.ErrUnavailable}

	resolver := users.NewProjectionResolver(users.ProjectionOptions{
		Projection: projection, Upstream: upstream, Logger: quiet(),
	})

	resolved, err := resolver.Resolve(context.Background(), []uuid.UUID{known, cold})
	// The error is reported so the caller can log the degradation...
	require.ErrorIs(t, err, users.ErrUnavailable)
	// ...but what the projection knew is still returned, and the page renders.
	require.Len(t, resolved, 1)
	require.Equal(t, "Alex K.", resolved[known].FullName)
	require.NotContains(t, resolved, cold)
}

// TestAFailingProjectionFallsThroughToIdentity. The projection lives in this
// service's own database, so a failure there is a real problem — but not this
// request's problem to die of.
func TestAFailingProjectionFallsThroughToIdentity(t *testing.T) {
	projection := newFakeProjection()
	projection.readErr = errors.New("connection refused")

	alex := uuid.New()
	upstream := &fakeUpstream{users: map[uuid.UUID]users.User{
		alex: {ID: alex, FullName: "Alex K.", RatingCount: 12},
	}}

	resolver := users.NewProjectionResolver(users.ProjectionOptions{
		Projection: projection, Upstream: upstream, Logger: quiet(),
	})

	resolved, err := resolver.Resolve(context.Background(), []uuid.UUID{alex})
	require.NoError(t, err)
	require.Equal(t, "Alex K.", resolved[alex].FullName)
	require.Equal(t, [][]uuid.UUID{{alex}}, upstream.askedFor)
}

// TestAFailingBackfillDoesNotFailThePage. The backfill is a cache warm. Nothing
// a page renders depends on it having worked.
func TestAFailingBackfillDoesNotFailThePage(t *testing.T) {
	projection := newFakeProjection()
	projection.backfillErr = errors.New("disk full")

	alex := uuid.New()
	upstream := &fakeUpstream{users: map[uuid.UUID]users.User{
		alex: {ID: alex, FullName: "Alex K.", RatingCount: 12},
	}}

	resolver := users.NewProjectionResolver(users.ProjectionOptions{
		Projection: projection, Upstream: upstream, Logger: quiet(),
	})

	resolved, err := resolver.Resolve(context.Background(), []uuid.UUID{alex})
	require.NoError(t, err)
	require.Equal(t, "Alex K.", resolved[alex].FullName)
}

// TestResolveDedupesAndDropsTheNilID, inherited from the client it wraps: a
// page listing eight trips by the same organizer must ask about that organizer
// once, and uuid.Nil is never a real user.
func TestResolveDedupesAndDropsTheNilID(t *testing.T) {
	projection := newFakeProjection()
	alex := uuid.New()
	projection.rows[alex] = domain.UserRef{UserID: alex, FullName: "Alex K."}

	resolver := users.NewProjectionResolver(users.ProjectionOptions{
		Projection: projection, Upstream: &fakeUpstream{}, Logger: quiet(),
	})

	resolved, err := resolver.Resolve(context.Background(), []uuid.UUID{alex, alex, uuid.Nil, alex})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	require.Equal(t, [][]uuid.UUID{{alex}}, projection.askedFor)

	empty, err := resolver.Resolve(context.Background(), []uuid.UUID{uuid.Nil})
	require.NoError(t, err)
	require.Empty(t, empty)
	require.Equal(t, 1, projection.reads, "an all-nil page does not touch the database")
}
