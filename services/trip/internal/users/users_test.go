package users_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/users"
)

// identityStub stands in for identity's GET /internal/users.
//
// It counts calls, because "one batch call per page" is the property this
// package exists to guarantee and the only way to assert it is to watch the
// wire. It also records the ids each call asked for, so a test can prove the
// second request asked only for what the cache did not have.
type identityStub struct {
	server *httptest.Server

	calls  atomic.Int64
	asked  [][]string
	users  map[uuid.UUID]users.User
	status int
	token  string
}

func newIdentityStub(t *testing.T) *identityStub {
	t.Helper()

	stub := &identityStub{
		users:  map[uuid.UUID]users.User{},
		status: http.StatusOK,
		token:  "test-internal-token",
	}

	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.calls.Add(1)
		require.Equal(t, "/internal/users", r.URL.Path)
		require.Equal(t, stub.token, r.Header.Get("X-Internal-Token"),
			"internal calls carry the shared secret from the environment")

		ids := strings.Split(r.URL.Query().Get("ids"), ",")
		stub.asked = append(stub.asked, ids)

		if stub.status != http.StatusOK {
			w.WriteHeader(stub.status)
			return
		}

		// Identity omits ids it does not know rather than erroring on them.
		found := make([]users.User, 0, len(ids))
		for _, raw := range ids {
			id, err := uuid.Parse(raw)
			if err != nil {
				continue
			}
			if user, known := stub.users[id]; known {
				found = append(found, user)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"users": found}))
	}))
	t.Cleanup(stub.server.Close)

	return stub
}

func (s *identityStub) add(fullName string) uuid.UUID {
	id := uuid.New()
	rating := 4.5
	photo := "https://example.test/" + id.String() + ".jpg"
	s.users[id] = users.User{
		ID: id, FullName: fullName, PhotoURL: &photo, RatingAvg: &rating, RatingCount: 12,
	}
	return id
}

func (s *identityStub) client(cache users.Cache) *users.Client {
	return users.New(users.Options{
		BaseURL:       s.server.URL,
		InternalToken: s.token,
		Timeout:       2 * time.Second,
		Cache:         cache,
	})
}

// memoryCache is the cache under test where Redis is not the subject: it has
// the same swallow-everything contract and lets a test assert on hits without a
// container.
type memoryCache struct {
	entries map[uuid.UUID]users.User
	reads   int
	writes  int
}

func newMemoryCache() *memoryCache {
	return &memoryCache{entries: map[uuid.UUID]users.User{}}
}

func (c *memoryCache) Get(_ context.Context, ids []uuid.UUID) map[uuid.UUID]users.User {
	c.reads++
	found := map[uuid.UUID]users.User{}
	for _, id := range ids {
		if user, ok := c.entries[id]; ok {
			found[id] = user
		}
	}
	return found
}

func (c *memoryCache) Put(_ context.Context, batch map[uuid.UUID]users.User) {
	c.writes++
	for id, user := range batch {
		c.entries[id] = user
	}
}

// TestResolveMakesOneCallForAWholePage is the rule identity's batch-only
// endpoint exists to enforce, checked from this side of the wire.
func TestResolveMakesOneCallForAWholePage(t *testing.T) {
	stub := newIdentityStub(t)

	ids := make([]uuid.UUID, 0, 20)
	for i := 0; i < 20; i++ {
		ids = append(ids, stub.add("Organizer"))
	}

	resolved, err := stub.client(users.NoCache{}).Resolve(context.Background(), ids)
	require.NoError(t, err)
	require.Len(t, resolved, 20)
	require.EqualValues(t, 1, stub.calls.Load(), "twenty organizers, one request")
}

func TestResolveDeduplicatesIds(t *testing.T) {
	stub := newIdentityStub(t)
	id := stub.add("Olena")

	// The page a search returns is often organised by a handful of people.
	repeated := []uuid.UUID{id, id, id, uuid.Nil, id}

	resolved, err := stub.client(users.NoCache{}).Resolve(context.Background(), repeated)
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	require.EqualValues(t, 1, stub.calls.Load())
	require.Equal(t, []string{id.String()}, stub.asked[0],
		"the nil uuid is never a user and is not asked about")
}

func TestResolveOmitsUnknownIds(t *testing.T) {
	stub := newIdentityStub(t)
	known := stub.add("Olena")
	deleted := uuid.New()

	resolved, err := stub.client(users.NoCache{}).Resolve(context.Background(), []uuid.UUID{known, deleted})
	require.NoError(t, err)

	require.Contains(t, resolved, known)
	require.NotContains(t, resolved, deleted,
		"a deleted account is an absence, not an error: the caller renders a placeholder")
}

func TestResolveServesTheSecondPageFromCache(t *testing.T) {
	stub := newIdentityStub(t)
	cache := newMemoryCache()
	client := stub.client(cache)

	first := stub.add("Olena")
	second := stub.add("Andrii")

	_, err := client.Resolve(context.Background(), []uuid.UUID{first})
	require.NoError(t, err)
	require.EqualValues(t, 1, stub.calls.Load())

	// The same organizer again: no request at all.
	resolved, err := client.Resolve(context.Background(), []uuid.UUID{first})
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	require.EqualValues(t, 1, stub.calls.Load())

	// A page with one new organizer asks only about the one it does not have.
	resolved, err = client.Resolve(context.Background(), []uuid.UUID{first, second})
	require.NoError(t, err)
	require.Len(t, resolved, 2)
	require.EqualValues(t, 2, stub.calls.Load())
	require.Equal(t, []string{second.String()}, stub.asked[1])
}

// TestResolveDegradesWhenIdentityIsUnreachable — the property the whole
// endpoint is built around.
func TestResolveDegradesWhenIdentityIsUnreachable(t *testing.T) {
	stub := newIdentityStub(t)
	id := stub.add("Olena")
	stub.server.Close()

	resolved, err := stub.client(users.NoCache{}).Resolve(context.Background(), []uuid.UUID{id})

	require.ErrorIs(t, err, users.ErrUnavailable)
	require.Empty(t, resolved, "nothing resolved, but nothing panicked and nothing was invented")
}

func TestResolveDegradesOnAnErrorStatus(t *testing.T) {
	stub := newIdentityStub(t)
	id := stub.add("Olena")
	stub.status = http.StatusInternalServerError

	resolved, err := stub.client(users.NoCache{}).Resolve(context.Background(), []uuid.UUID{id})
	require.ErrorIs(t, err, users.ErrUnavailable)
	require.Empty(t, resolved)
}

// TestResolveReturnsCacheHitsWhenIdentityIsDown: a partial answer is better
// than none. The organizers already in the cache still render.
func TestResolveReturnsCacheHitsWhenIdentityIsDown(t *testing.T) {
	stub := newIdentityStub(t)
	cache := newMemoryCache()
	client := stub.client(cache)

	cached := stub.add("Olena")
	fresh := stub.add("Andrii")

	_, err := client.Resolve(context.Background(), []uuid.UUID{cached})
	require.NoError(t, err)

	stub.server.Close()

	resolved, err := client.Resolve(context.Background(), []uuid.UUID{cached, fresh})
	require.ErrorIs(t, err, users.ErrUnavailable)
	require.Contains(t, resolved, cached)
	require.NotContains(t, resolved, fresh)
}

func TestResolveTimesOut(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)

	client := users.New(users.Options{
		BaseURL:       slow.URL,
		InternalToken: "t",
		// A search page cannot wait on identity: past this, it renders without
		// organizer names.
		Timeout: 100 * time.Millisecond,
	})

	started := time.Now()
	_, err := client.Resolve(context.Background(), []uuid.UUID{uuid.New()})
	require.ErrorIs(t, err, users.ErrUnavailable)
	require.Less(t, time.Since(started), 2*time.Second)
}

func TestResolveWithNoIdsMakesNoCall(t *testing.T) {
	stub := newIdentityStub(t)

	resolved, err := stub.client(users.NoCache{}).Resolve(context.Background(), nil)
	require.NoError(t, err)
	require.Empty(t, resolved)
	require.EqualValues(t, 0, stub.calls.Load())
}

func TestResolveDoesNotCacheAFailedFetch(t *testing.T) {
	stub := newIdentityStub(t)
	cache := newMemoryCache()
	id := stub.add("Olena")
	stub.status = http.StatusServiceUnavailable

	_, err := stub.client(cache).Resolve(context.Background(), []uuid.UUID{id})
	require.Error(t, err)
	require.Zero(t, cache.writes, "an outage must not be cached for sixty seconds")
}

func TestResolveSendsIdsAsOneCommaSeparatedParameter(t *testing.T) {
	stub := newIdentityStub(t)
	first, second := stub.add("Olena"), stub.add("Andrii")

	_, err := stub.client(users.NoCache{}).Resolve(context.Background(), []uuid.UUID{first, second})
	require.NoError(t, err)

	// The shape identity's InternalUsersQuery parses: one `ids` parameter, not
	// one per id.
	require.Len(t, stub.asked, 1)
	require.Equal(t, []string{first.String(), second.String()}, stub.asked[0])

	_, err = url.Parse(stub.server.URL)
	require.NoError(t, err)
}
