// Package realtime is everything in this service that lives in Redis: the
// websocket ticket store, the per-room fan-out, the presence set and the
// message rate limiter.
//
// It is grouped by *where the state lives* rather than by feature, because that
// is the property that matters here. All four are shared between replicas and
// none of them is in Postgres, and keeping them in one package makes the key
// namespace visible in one file listing instead of scattered across the
// service.
package realtime

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/togethergo/chat/internal/domain"
)

// Key namespaces. Redis is shared between services (one container, CLAUDE.md),
// so every key says who wrote it.
const (
	ticketPrefix   = "chat:ticket:"
	roomPrefix     = "chat:room:"
	presencePrefix = "chat:presence:"
	ratePrefix     = "chat:rate:"
)

// ticketBytes is the entropy in a ticket. Thirty-two bytes, which is 256 bits
// and the same size as the opaque refresh tokens identity mints — a value
// nobody is going to guess inside its thirty-second life, and a size that
// nobody has to think about again.
const ticketBytes = 32

// Ticket is what a websocket handshake presents instead of a bearer token.
//
// The browser WebSocket API cannot set an Authorization header. The obvious
// workaround is to put the access token in the query string, and it is a bad
// one: query strings are written to the gateway's access log, to the browser's
// history, and to the Referer header of anything the page loads afterwards, and
// the token is good for fifteen minutes across every service in the system. A
// ticket is good for thirty seconds, for one room, once.
type Ticket struct {
	// Value is the credential itself, base64url of 32 random bytes. It is the
	// Redis key, not a stored field: the only copies of it are in the client's
	// memory and in the URL of the handshake it is about to make.
	Value string

	// TTL is how long it is redeemable, echoed to the client as `expires_in`.
	TTL time.Duration
}

// ticketClaims is what the ticket resolves to. Stored as the Redis *value*, so
// that redeeming a ticket is a single lookup and there is nothing in the ticket
// string itself for a client to read or tamper with.
type ticketClaims struct {
	UserID uuid.UUID `json:"user_id"`
	TripID uuid.UUID `json:"trip_id"`
}

// Tickets issues and redeems websocket tickets.
type Tickets struct {
	client *redis.Client
	ttl    time.Duration
}

// NewTickets builds the ticket store over an already-configured client. The
// client's lifetime belongs to the caller.
func NewTickets(client *redis.Client, ttl time.Duration) *Tickets {
	return &Tickets{client: client, ttl: ttl}
}

// Issue mints a ticket for one user and one trip.
//
// The caller has already verified membership; this function does not, and
// deliberately does not know how. What it guarantees is that the ticket it
// returns resolves to exactly the pair it was given — which is the property
// Redeem depends on.
func (t *Tickets) Issue(ctx context.Context, userID, tripID uuid.UUID) (Ticket, error) {
	raw := make([]byte, ticketBytes)
	if _, err := rand.Read(raw); err != nil {
		return Ticket{}, fmt.Errorf("generate ticket: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(raw)

	claims, err := json.Marshal(ticketClaims{UserID: userID, TripID: tripID})
	if err != nil {
		return Ticket{}, fmt.Errorf("encode ticket claims: %w", err)
	}

	// SetNX rather than Set: a collision on 256 bits of entropy is not going to
	// happen, but overwriting somebody else's live ticket if it did would be a
	// silent authentication failure for them, and the NX costs nothing.
	stored, err := t.client.SetNX(ctx, ticketPrefix+value, claims, t.ttl).Result()
	if err != nil {
		return Ticket{}, fmt.Errorf("store ticket: %w", err)
	}
	if !stored {
		return Ticket{}, errors.New("ticket key collision")
	}
	return Ticket{Value: value, TTL: t.ttl}, nil
}

// Redeem spends a ticket and returns the user it belongs to.
//
// GETDEL, which is one command and therefore atomic: the read and the delete
// cannot be interleaved by a second connection presenting the same ticket, so
// exactly one of two racing handshakes gets the user id and the other gets
// nothing. A GET followed by a DEL would let both in, which is the whole
// failure mode single use exists to prevent.
//
// The trip is re-checked here rather than trusted from the URL. A ticket issued
// for trip A and presented on trip B's socket is refused, so a member of any
// one room cannot use their legitimate ticket to open any other.
//
// Every failure returns domain.ErrTicketInvalid: missing, expired, already
// spent and wrong-trip are one answer, because the client's next move is
// identical in all four cases and telling it which would only help someone
// probing the endpoint.
func (t *Tickets) Redeem(ctx context.Context, value string, tripID uuid.UUID) (uuid.UUID, error) {
	if value == "" {
		return uuid.Nil, domain.ErrTicketInvalid
	}

	stored, err := t.client.GetDel(ctx, ticketPrefix+value).Bytes()
	if errors.Is(err, redis.Nil) {
		return uuid.Nil, domain.ErrTicketInvalid
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("redeem ticket: %w", err)
	}

	var claims ticketClaims
	if err := json.Unmarshal(stored, &claims); err != nil {
		return uuid.Nil, fmt.Errorf("decode ticket claims: %w", err)
	}
	if claims.TripID != tripID || claims.UserID == uuid.Nil {
		return uuid.Nil, domain.ErrTicketInvalid
	}
	return claims.UserID, nil
}
