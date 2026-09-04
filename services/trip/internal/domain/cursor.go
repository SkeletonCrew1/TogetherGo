package domain

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Cursor is the position of the last row of a page, keyed on (start_at, id).
//
// One type for both directions. Discovery sorts (start_at ASC, id ASC) — the
// soonest departure first, because it is a feed of trips you can still join —
// and the dashboard sorts (start_at DESC, id DESC), because it is a history.
// The *position* either one resumes from is the same pair of values; only the
// comparison operator differs, and that belongs to the query, not to the token.
// Encoding them identically is what lets one parser and one wire format serve
// every trip listing in the service.
//
// Both halves are needed. start_at alone is not unique — several trips can
// leave at the same instant — so a cursor holding only the timestamp either
// skips rows (`>`) or repeats them forever (`>=`). The id is what makes the
// ordering total, and it is the same tiebreaker the trips_status_start_idx
// index carries so the comparison stays an index qual rather than a filter.
//
// OFFSET would need none of this and is not used, here or anywhere: it makes
// the database count and discard every row of every preceding page, and it
// silently drops or repeats rows when a trip is created between two requests.
type Cursor struct {
	StartAt time.Time
	ID      uuid.UUID
}

// cursorSeparator cannot occur in either half: RFC 3339 has no '|', and neither
// does a uuid. That is what makes decoding unambiguous.
const cursorSeparator = "|"

// cursorMaxLen bounds the encoded form before anything decodes it. A real
// cursor is 72 characters; this is generous and still small enough that a
// client sending a megabyte in the query string is rejected by a length check
// rather than by an allocator.
const cursorMaxLen = 256

// Encode renders the cursor as the opaque string clients echo back.
//
// RFC3339Nano, not RFC3339: timestamptz keeps microseconds, and a cursor
// truncated to the second would re-read every row that shares that second with
// the last row of the page. Base64 is not encryption and is not meant to be —
// it says "this is our string, not yours" and keeps a timestamp with a '+' in
// it out of a query string, nothing more.
func (c Cursor) Encode() string {
	raw := c.StartAt.UTC().Format(time.RFC3339Nano) + cursorSeparator + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses what Encode produced, and rejects everything else with a
// validation error — never a panic.
//
// Every step is a failure the client controls: the length, the alphabet, the
// separator, the timestamp and the uuid. None of them may index into a slice
// that a previous step has not proved is there, which is why the separator is
// found with Cut rather than by splitting and trusting the count.
func DecodeCursor(raw string) (*Cursor, error) {
	invalid := func(reason string) error {
		return &ValidationError{Fields: []FieldError{{
			Field:   "cursor",
			Message: "is not a valid cursor (" + reason + "); omit it to start from the first page",
		}}}
	}

	if raw == "" {
		return nil, nil
	}
	if len(raw) > cursorMaxLen {
		return nil, invalid(fmt.Sprintf("longer than %d characters", cursorMaxLen))
	}

	decoded, err := decodeCursorBase64(raw)
	if err != nil {
		return nil, invalid("not base64")
	}

	startAt, id, found := strings.Cut(string(decoded), cursorSeparator)
	if !found {
		return nil, invalid("wrong shape")
	}

	parsedStart, err := time.Parse(time.RFC3339Nano, startAt)
	if err != nil {
		return nil, invalid("bad timestamp")
	}
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return nil, invalid("bad id")
	}

	return &Cursor{StartAt: parsedStart.UTC(), ID: parsedID}, nil
}

// decodeCursorBase64 accepts both alphabets and both padding conventions.
//
// We only ever emit unpadded base64url, but cursors travel through client code,
// URL builders and copy-paste, and any of those may re-encode. Accepting the
// standard alphabet costs one extra attempt and turns a class of spurious 400s
// into working pagination.
func decodeCursorBase64(raw string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.URLEncoding.DecodeString(raw); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		return decoded, nil
	}
	return base64.StdEncoding.DecodeString(raw)
}
