package realtime

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Presence is who is currently in a room, across every replica.
//
// It is a sorted set rather than a plain set, and that is the only interesting
// decision in this file. A set has one expiry for the whole key, so a member
// left behind by a killed instance stays "online" until the entire room's
// presence expires and everyone appears to leave at once. A sorted set scored
// by the member's last heartbeat gives each member its own expiry: sweeping is
// a range delete by score, and a crashed instance's users drain out one TTL
// after their last heartbeat while everybody else is untouched.
//
// Presence is best-effort by construction. It lives only in Redis, it is
// rebuilt by heartbeats within one interval of anything going wrong, and no
// decision in this service is made from it — a user is allowed into a room by
// room_members, never by this set.
type Presence struct {
	client *redis.Client
	ttl    time.Duration
}

// defaultPresenceTTL is the fallback when a caller passes a non-positive one.
//
// Not defensiveness for its own sake: Touch sets an expiry on the whole key,
// and Redis treats an EXPIRE of zero as a DELETE. A zero TTL would therefore
// erase a room's presence on every heartbeat, which is a subtle enough failure
// to be worth ruling out here rather than relying on config validation alone.
const defaultPresenceTTL = 90 * time.Second

// NewPresence builds the presence store over an already-configured client.
func NewPresence(client *redis.Client, ttl time.Duration) *Presence {
	if ttl <= 0 {
		ttl = defaultPresenceTTL
	}
	return &Presence{client: client, ttl: ttl}
}

func presenceKey(tripID uuid.UUID) string { return presencePrefix + tripID.String() }

// Touch records that a user is present in a room, and reports whether this is
// news.
//
// The boolean is what drives the `presence` frame. ZADD returns the number of
// members it *added* as opposed to updated, so the first heartbeat for a user
// returns true and the next thirty return false — which means the caller can
// announce an arrival without keeping any state of its own about who it has
// already announced. It is also what makes the failure case self-healing: if
// another replica sweeps a user out while they are still connected here, this
// instance's next heartbeat re-adds them, sees `true`, and announces them
// again.
func (p *Presence) Touch(ctx context.Context, tripID, userID uuid.UUID) (bool, error) {
	key := presenceKey(tripID)

	added, err := p.client.ZAdd(ctx, key, redis.Z{
		Score:  float64(time.Now().Unix()),
		Member: userID.String(),
	}).Result()
	if err != nil {
		return false, fmt.Errorf("record presence for %s in room %s: %w", userID, tripID, err)
	}

	// An expiry on the whole key as well, so a room nobody has opened for a
	// while stops occupying memory. Generous, because the per-member scores are
	// what actually expire members; this only reaps the empty husk.
	if err := p.client.Expire(ctx, key, p.ttl*10).Err(); err != nil {
		return added == 1, fmt.Errorf("set presence expiry for room %s: %w", tripID, err)
	}
	return added == 1, nil
}

// Forget removes a user from a room's presence, and reports whether this call
// is the one that removed them.
//
// The boolean again: ZREM returns how many members it removed, so two replicas
// racing to drop the same user produce exactly one "offline" announcement.
func (p *Presence) Forget(ctx context.Context, tripID, userID uuid.UUID) (bool, error) {
	removed, err := p.client.ZRem(ctx, presenceKey(tripID), userID.String()).Result()
	if err != nil {
		return false, fmt.Errorf("clear presence for %s in room %s: %w", userID, tripID, err)
	}
	return removed == 1, nil
}

// Sweep drops members whose last heartbeat is older than the TTL and returns
// the ones it actually removed.
//
// Read-then-remove-individually rather than a single ZREMRANGEBYSCORE, because
// the caller has to announce each departure and a range delete only reports a
// count. Removing one at a time also makes the announcement correct when two
// replicas sweep the same room at the same moment: each user is removed by
// exactly one of them, and only that one announces.
func (p *Presence) Sweep(ctx context.Context, tripID uuid.UUID) ([]uuid.UUID, error) {
	key := presenceKey(tripID)
	cutoff := strconv.FormatInt(time.Now().Add(-p.ttl).Unix(), 10)

	stale, err := p.client.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: "-inf",
		Max: "(" + cutoff,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("find stale presence in room %s: %w", tripID, err)
	}

	swept := make([]uuid.UUID, 0, len(stale))
	for _, member := range stale {
		userID, err := uuid.Parse(member)
		if err != nil {
			// Not something this service wrote. Drop it and move on rather
			// than failing the sweep for every other member of the room.
			_ = p.client.ZRem(ctx, key, member).Err()
			continue
		}
		removed, err := p.client.ZRem(ctx, key, member).Result()
		if err != nil {
			return swept, fmt.Errorf("sweep presence in room %s: %w", tripID, err)
		}
		if removed == 1 {
			swept = append(swept, userID)
		}
	}
	return swept, nil
}

// Online lists who is currently in a room, ignoring members whose heartbeat has
// lapsed.
//
// The score filter is applied on read rather than relying on a sweep having run
// first, so a socket connecting to a room nothing has swept recently still gets
// an accurate snapshot rather than a list including whoever was there when an
// instance was killed.
func (p *Presence) Online(ctx context.Context, tripID uuid.UUID) ([]uuid.UUID, error) {
	cutoff := strconv.FormatInt(time.Now().Add(-p.ttl).Unix(), 10)

	members, err := p.client.ZRangeByScore(ctx, presenceKey(tripID), &redis.ZRangeBy{
		Min: cutoff,
		Max: "+inf",
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("read presence in room %s: %w", tripID, err)
	}

	online := make([]uuid.UUID, 0, len(members))
	for _, member := range members {
		if userID, err := uuid.Parse(member); err == nil {
			online = append(online, userID)
		}
	}
	return online, nil
}
