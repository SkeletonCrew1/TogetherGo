// Package users resolves user ids to the display data a trip listing needs.
//
// The trip service does not own users and has no copy of them: identity's
// database is identity's (CLAUDE.md rule 1), and there is no join across that
// boundary. What there is instead is `GET /internal/users`, a batch resolver on
// the internal network, and a short-lived cache in front of it.
//
// Two rules govern everything in this package:
//
//   - **One call per page.** Identity offers no single-id variant precisely so
//     that nobody writes the loop; Resolve takes a slice and makes one request
//     for whatever the cache did not already have.
//   - **A failure here is not a failure of the page.** Discovery is the
//     highest-traffic read path in the system and its results are the trips,
//     not their organizers. If identity is unreachable, Resolve returns what it
//     has and says so; the caller renders the rest with a null name. A search
//     page must never answer 500 because a different service is down.
package users

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// User is the projection identity returns from its batch resolver. Adding a
// field here means identity added one first — see its InternalUser schema.
type User struct {
	ID          uuid.UUID `json:"id"`
	FullName    string    `json:"full_name"`
	PhotoURL    *string   `json:"photo_url"`
	RatingAvg   *float64  `json:"rating_avg"`
	RatingCount int       `json:"rating_count"`
}

// Resolver is the dependency the HTTP layer holds. An interface so that the
// handler tests can drive the degraded path without an unreachable port, and
// so that a service built without an identity URL can hold a resolver that
// resolves nothing rather than a nil pointer.
type Resolver interface {
	Resolve(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]User, error)
}

// maxBatch mirrors MAX_INTERNAL_IDS in identity's schema. A discovery page is
// at most domain.SearchLimitMax trips and therefore at most that many distinct
// organizers, so the chunking below never runs more than once in practice; it
// is here so that a future caller with a longer list degrades into two requests
// instead of a 400.
const maxBatch = 100

// ErrUnavailable is what Resolve reports when identity could not be reached or
// answered with something other than 200. Callers match on it to log a warning
// and carry on — nothing in this service treats it as fatal.
var ErrUnavailable = errors.New("identity service unavailable")

// Client is the resolver backed by identity plus a cache.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	cache   Cache
	logger  *slog.Logger
}

// Options configures a Client. BaseURL is identity's internal origin — the
// container address, not the gateway: /internal is deliberately unroutable from
// outside the Compose network.
type Options struct {
	BaseURL       string
	InternalToken string
	Timeout       time.Duration
	Cache         Cache
	Logger        *slog.Logger
}

// New builds a client. A nil cache is replaced with one that stores nothing, so
// there is no "is caching configured" branch anywhere else.
func New(opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	cache := opts.Cache
	if cache == nil {
		cache = NoCache{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}

	return &Client{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		token:   opts.InternalToken,
		// A short timeout on purpose. This call sits inside a page render, and
		// a slow identity must degrade to "no organizer names" quickly rather
		// than hold every search request open until the client gives up.
		http:   &http.Client{Timeout: timeout},
		cache:  cache,
		logger: logger,
	}
}

// Resolve returns what is known about each id.
//
// Ids that identity does not recognise — a deleted account still referenced by
// an old trip — are simply absent from the result, which is identity's contract
// and not an error. So is a partial result after a failure: the map holds every
// cache hit, and the error says the rest could not be fetched.
func (c *Client) Resolve(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]User, error) {
	wanted := dedupe(ids)
	if len(wanted) == 0 {
		return map[uuid.UUID]User{}, nil
	}

	resolved := c.cache.Get(ctx, wanted)
	missing := make([]uuid.UUID, 0, len(wanted))
	for _, id := range wanted {
		if _, hit := resolved[id]; !hit {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return resolved, nil
	}

	fetched := make(map[uuid.UUID]User, len(missing))
	var failure error
	for _, batch := range chunk(missing, maxBatch) {
		users, err := c.fetch(ctx, batch)
		if err != nil {
			failure = err
			break
		}
		for _, user := range users {
			fetched[user.ID] = user
			resolved[user.ID] = user
		}
	}

	// Written after the loop rather than inside it: one cache round trip per
	// request, and nothing is stored if the fetch failed halfway.
	if len(fetched) > 0 {
		c.cache.Put(ctx, fetched)
	}
	return resolved, failure
}

func (c *Client) fetch(ctx context.Context, ids []uuid.UUID) ([]User, error) {
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = id.String()
	}

	endpoint := c.baseURL + "/internal/users?" + url.Values{"ids": {strings.Join(raw, ",")}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrUnavailable, err)
	}
	// The shared secret from the environment. /internal is not routed through
	// the gateway; this is the second line of defence behind that, because in
	// Compose every service shares one network.
	req.Header.Set("X-Internal-Token", c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		// The body is not read into the error: it belongs to another service
		// and could carry anything. The status is what this service can act on.
		return nil, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}

	var body struct {
		Users []User `json:"users"`
	}
	// Bounded: a malformed or hostile upstream must not be able to make this
	// service allocate without limit. A hundred users is a few tens of KiB.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrUnavailable, err)
	}
	return body.Users, nil
}

// dedupe preserves order and drops the nil uuid, which is what a trip with an
// unknown organizer would carry and is never a real user.
func dedupe(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func chunk(ids []uuid.UUID, size int) [][]uuid.UUID {
	batches := make([][]uuid.UUID, 0, (len(ids)+size-1)/size)
	for start := 0; start < len(ids); start += size {
		end := min(start+size, len(ids))
		batches = append(batches, ids[start:end])
	}
	return batches
}
