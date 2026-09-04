package domain

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Room statuses. Two, and only two: this service is bound to four routing keys
// and hears about no other transition — see the migration for why.
const (
	// StatusRecruiting is a room that accepts messages. It is named after the
	// trip status `trip.created` reports rather than something chat-specific
	// like "open", so that a row in this table can be read against the trip
	// service's own vocabulary without a translation table.
	StatusRecruiting = "recruiting"

	// StatusCancelled is a called-off trip. The room stays readable forever;
	// nothing new may be written to it.
	StatusCancelled = "cancelled"
)

// Room is a trip's conversation.
type Room struct {
	TripID    uuid.UUID
	Title     string
	Status    string
	CreatedAt time.Time
}

// IsOpen reports whether new messages are accepted.
func (r Room) IsOpen() bool { return r.Status != StatusCancelled }

// Message is one thing somebody said.
type Message struct {
	ID        int64
	TripID    uuid.UUID
	SenderID  uuid.UUID
	Body      string
	CreatedAt time.Time
}

// RoomSummary is a row of GET /api/chat/rooms: the room, what was said in it
// last, and how much of that the caller has not read.
type RoomSummary struct {
	Room Room

	// LastMessage is nil in a room where nobody has said anything yet, which is
	// every room between `trip.created` and the organizer's first word.
	LastMessage *Message

	UnreadCount int

	// LastActivity is the last message's timestamp, or the room's creation
	// time when there is none. It is the sort key, and half of the pagination
	// cursor, so it is computed once in SQL rather than three times in Go.
	LastActivity time.Time
}

// Message body limits, in runes rather than bytes.
//
// A 2000-byte limit would let an English speaker write 2000 characters and a
// Ukrainian one about 1000, which is not a rule anybody would write down on
// purpose. utf8.RuneCountInString is what the check below counts, and the
// database's CHECK uses char_length for the same reason.
const (
	MinBodyLength = 1
	MaxBodyLength = 2000
)

// CleanBody trims a message body and checks it against the length rule.
//
// Trimming first is the whole point: a body of three spaces is empty, not three
// characters long, and a client that sends one has a bug in its composer rather
// than something to say. Leading and trailing whitespace is removed on the way
// in so that every reader of the table sees the same thing — normalising on
// read would mean every consumer of the row has to remember to.
func CleanBody(raw string) (string, error) {
	body := strings.TrimSpace(raw)

	if utf8.RuneCountInString(body) < MinBodyLength {
		return "", invalid("body", "must not be empty or whitespace only")
	}
	if utf8.RuneCountInString(body) > MaxBodyLength {
		return "", invalid("body", "must be at most 2000 characters")
	}
	return body, nil
}

// PageLimit is the size of a history page.
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 100
)

// CleanLimit validates a `limit` query parameter, defaulting when it is absent.
//
// A limit of zero is treated as absent rather than as "give me nothing": every
// client that omits the parameter and every client that sends an empty one mean
// the same thing, and answering one of them with an empty page would be a
// puzzle rather than a service.
func CleanLimit(raw int) (int, error) {
	if raw == 0 {
		return DefaultPageLimit, nil
	}
	if raw < 0 {
		return 0, invalid("limit", "must be a positive integer")
	}
	if raw > MaxPageLimit {
		return 0, invalid("limit", "must be at most 100")
	}
	return raw, nil
}
