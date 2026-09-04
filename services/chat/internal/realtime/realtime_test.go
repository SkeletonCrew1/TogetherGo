package realtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/chat/internal/domain"
	"github.com/togethergo/chat/internal/realtime"
	"github.com/togethergo/chat/internal/testsupport"
)

// A ticket is redeemable once, by whoever gets there first.
//
// This is what GETDEL buys and why the redemption is one command: a GET
// followed by a DEL would let two handshakes racing on the same value both
// succeed, which is the whole failure mode single use exists to prevent.
func TestATicketIsSingleUse(t *testing.T) {
	tickets := realtime.NewTickets(testsupport.Redis(t), 30*time.Second)
	ctx := context.Background()

	userID, tripID := uuid.New(), uuid.New()

	ticket, err := tickets.Issue(ctx, userID, tripID)
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, ticket.TTL)

	redeemed, err := tickets.Redeem(ctx, ticket.Value, tripID)
	require.NoError(t, err)
	require.Equal(t, userID, redeemed)

	_, err = tickets.Redeem(ctx, ticket.Value, tripID)
	require.ErrorIs(t, err, domain.ErrTicketInvalid)
}

// A ticket is for one room. Membership of any trip must not become membership
// of all of them.
func TestATicketDoesNotOpenAnotherTripsRoom(t *testing.T) {
	tickets := realtime.NewTickets(testsupport.Redis(t), 30*time.Second)
	ctx := context.Background()

	ticket, err := tickets.Issue(ctx, uuid.New(), uuid.New())
	require.NoError(t, err)

	_, err = tickets.Redeem(ctx, ticket.Value, uuid.New())
	require.ErrorIs(t, err, domain.ErrTicketInvalid)
}

// Missing, expired and never-issued are one answer, because the client's next
// move is identical in all three and distinguishing them only helps somebody
// probing the endpoint.
func TestAnUnknownTicketIsRefused(t *testing.T) {
	tickets := realtime.NewTickets(testsupport.Redis(t), 30*time.Second)
	ctx := context.Background()

	for _, value := range []string{"", "not-a-ticket", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		_, err := tickets.Redeem(ctx, value, uuid.New())
		require.ErrorIs(t, err, domain.ErrTicketInvalid, "value %q", value)
	}
}

func TestATicketExpires(t *testing.T) {
	tickets := realtime.NewTickets(testsupport.Redis(t), 100*time.Millisecond)
	ctx := context.Background()

	ticket, err := tickets.Issue(ctx, uuid.New(), uuid.New())
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)

	_, err = tickets.Redeem(ctx, ticket.Value, uuid.New())
	require.ErrorIs(t, err, domain.ErrTicketInvalid)
}

// The limit is per user *per room*, and the window is anchored to the first
// message rather than to a wall-clock bucket — so twenty-one in quick
// succession are twenty-one in one window, with no boundary for the last one to
// slip through.
func TestTheRateLimiterCountsPerUserPerRoom(t *testing.T) {
	client := testsupport.Redis(t)
	limiter := realtime.NewLimiter(client, 20, 10*time.Second)
	ctx := context.Background()

	tripID, userID, otherUser := uuid.New(), uuid.New(), uuid.New()

	for i := 1; i <= 20; i++ {
		allowed, err := limiter.Allow(ctx, tripID, userID)
		require.NoError(t, err)
		require.True(t, allowed, "message %d was inside the limit", i)
	}

	allowed, err := limiter.Allow(ctx, tripID, userID)
	require.NoError(t, err)
	require.False(t, allowed, "the twenty-first message is over the limit")

	// Another user in the same room has their own budget.
	allowed, err = limiter.Allow(ctx, tripID, otherUser)
	require.NoError(t, err)
	require.True(t, allowed)

	// And so does the same user in another room.
	allowed, err = limiter.Allow(ctx, uuid.New(), userID)
	require.NoError(t, err)
	require.True(t, allowed)
}

// The window expires on its own, so being over the limit is temporary without
// anything having to reset it.
func TestTheRateLimitWindowExpires(t *testing.T) {
	limiter := realtime.NewLimiter(testsupport.Redis(t), 2, 200*time.Millisecond)
	ctx := context.Background()

	tripID, userID := uuid.New(), uuid.New()

	for i := 0; i < 2; i++ {
		allowed, err := limiter.Allow(ctx, tripID, userID)
		require.NoError(t, err)
		require.True(t, allowed)
	}
	allowed, err := limiter.Allow(ctx, tripID, userID)
	require.NoError(t, err)
	require.False(t, allowed)

	time.Sleep(400 * time.Millisecond)

	allowed, err = limiter.Allow(ctx, tripID, userID)
	require.NoError(t, err)
	require.True(t, allowed, "the window expired on its own")
}

// Presence reports whether a Touch was news, which is what lets the hub
// announce an arrival without keeping any state of its own about who it has
// already announced.
func TestPresenceReportsArrivalsAndDepartures(t *testing.T) {
	presence := realtime.NewPresence(testsupport.Redis(t), time.Minute)
	ctx := context.Background()

	tripID, userID := uuid.New(), uuid.New()

	arrived, err := presence.Touch(ctx, tripID, userID)
	require.NoError(t, err)
	require.True(t, arrived, "the first heartbeat is an arrival")

	arrived, err = presence.Touch(ctx, tripID, userID)
	require.NoError(t, err)
	require.False(t, arrived, "later heartbeats are not")

	online, err := presence.Online(ctx, tripID)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{userID}, online)

	departed, err := presence.Forget(ctx, tripID, userID)
	require.NoError(t, err)
	require.True(t, departed)

	departed, err = presence.Forget(ctx, tripID, userID)
	require.NoError(t, err)
	require.False(t, departed, "two replicas racing to remove one user announce once")
}

// A member whose instance was killed stops heartbeating and is swept out. This
// is the case no amount of deferred cleanup can cover, and it is why presence
// is a sorted set with per-member scores rather than a plain set.
func TestPresenceSweepsMembersWhoStoppedHeartbeating(t *testing.T) {
	// One second, so "stopped heartbeating a while ago" costs the test two
	// seconds instead of a minute. Presence scores are second-granular unix
	// timestamps, which is why the sleep below is comfortably over two.
	presence := realtime.NewPresence(testsupport.Redis(t), time.Second)
	ctx := context.Background()

	tripID, userID := uuid.New(), uuid.New()
	_, err := presence.Touch(ctx, tripID, userID)
	require.NoError(t, err)

	time.Sleep(2200 * time.Millisecond)

	swept, err := presence.Sweep(ctx, tripID)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{userID}, swept)

	// And a second sweep finds nothing, so the departure is announced once.
	swept, err = presence.Sweep(ctx, tripID)
	require.NoError(t, err)
	require.Empty(t, swept)
}

// The fan-out, on its own: what one instance publishes, another receives.
func TestTheBusDeliversToASubscriber(t *testing.T) {
	client := testsupport.Redis(t)
	logger := testsupport.DiscardLogger()

	publisher := realtime.NewBus(client, logger)
	subscriber := realtime.NewBus(client, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan string, 4)
	go subscriber.Run(ctx, func(tripID uuid.UUID, payload []byte) {
		received <- tripID.String() + ":" + string(payload)
	})

	tripID := uuid.New()
	require.NoError(t, subscriber.Subscribe(ctx, tripID))

	// Redis subscriptions are established asynchronously, so the publish is
	// retried until one lands rather than sent once and hoped for. A test that
	// published exactly once here would be flaky, and the flake would be in the
	// test rather than in the code.
	require.Eventually(t, func() bool {
		require.NoError(t, publisher.Publish(ctx, tripID, []byte("hello")))
		select {
		case got := <-received:
			require.Equal(t, tripID.String()+":hello", got)
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}, 10*time.Second, 10*time.Millisecond)

	// And an unsubscribed room's traffic stops arriving.
	require.NoError(t, subscriber.Unsubscribe(ctx, tripID))
	time.Sleep(200 * time.Millisecond)
	drain(received)

	require.NoError(t, publisher.Publish(ctx, tripID, []byte("nobody is listening")))
	select {
	case got := <-received:
		t.Fatalf("received %q after unsubscribing", got)
	case <-time.After(300 * time.Millisecond):
	}
}

func drain(ch chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
