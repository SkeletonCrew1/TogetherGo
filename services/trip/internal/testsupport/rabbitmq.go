package testsupport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Broker is a throwaway RabbitMQ for the relay tests.
//
// It runs the same image Compose runs and boots with the repository's real
// deploy/rabbitmq/definitions.json, so the exchange, the queues and the
// bindings under test are the ones production uses rather than a topology the
// test invented to agree with itself. In particular `notification.events` is
// already bound to `join_request.*` and `trip.invite_sent`, which is how a test
// can assert that a published event actually reaches a consumer instead of
// merely reaching the exchange.
//
// Nothing here declares anything. That is the same rule the services follow
// (contracts/events.md): the broker owns the topology and a client that
// declares its own will eventually declare it with different arguments.
type Broker struct {
	// URL is the amqp DSN, stable across Stop and Start.
	URL string

	t         *testing.T
	container testcontainers.Container
}

// The queues the trip service's events land in, named here so a test says which
// consumer's inbox it is reading rather than repeating a string.
const (
	NotificationQueue = "notification.events"
	ChatQueue         = "chat.trip-events"

	// UserQueue is this service's own inbox and UserDLQ is where its poison
	// messages land. Named here for the consumer tests, which purge the first
	// and read the second.
	UserQueue = "trip.user-events"
	UserDLQ   = "trip.user-events.dlq"
)

const (
	brokerImage = "rabbitmq:3.13-management"
	brokerUser  = "togethergo"
	brokerPass  = "togethergo"
)

// sharedBroker is built once per test binary. Starting a container per test
// would dominate the runtime, and unlike a database there is nothing to reset
// between tests but a queue — see Purge.
var sharedBroker struct {
	broker *Broker
	err    error
	once   sync.Once
}

// NewBroker returns the test binary's broker, starting it on the first call.
//
// The container outlives the test that started it and is reaped by
// testcontainers' Ryuk when the binary exits — the same arrangement the
// Postgres helper uses, and for the same reason.
//
// The host port is pinned rather than left to Docker. A relay under test holds
// one URL for its whole life, and a dynamically mapped port changes when the
// container is stopped and started again — which is precisely the scenario the
// reconnection test exists to exercise, and would otherwise fail for the wrong
// reason.
func NewBroker(t *testing.T) *Broker {
	t.Helper()

	if testing.Short() {
		t.Skip("broker tests start a container; skipped under -short")
	}

	sharedBroker.once.Do(func() {
		sharedBroker.broker, sharedBroker.err = startBroker()
	})
	if sharedBroker.err != nil {
		t.Fatalf("start rabbitmq container: %v", sharedBroker.err)
	}

	// The *testing.T is rebound each call so a failure inside Consume or await
	// is reported against the test that is running, not against the one that
	// happened to start the container.
	sharedBroker.broker.t = t
	return sharedBroker.broker
}

func startBroker() (*Broker, error) {
	port, err := freeBrokerPort()
	if err != nil {
		return nil, err
	}
	definitions, conf, err := brokerConfigFiles()
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	created, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        brokerImage,
			ExposedPorts: []string{"5672/tcp"},
			Files: []testcontainers.ContainerFile{
				{HostFilePath: conf, ContainerFilePath: "/etc/rabbitmq/rabbitmq.conf", FileMode: 0o644},
				{HostFilePath: definitions, ContainerFilePath: "/etc/rabbitmq/definitions.json", FileMode: 0o644},
			},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.PortBindings = nat.PortMap{
					"5672/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: strconv.Itoa(port)}},
				}
			},
			WaitingFor: wait.ForListeningPort("5672/tcp").WithStartupTimeout(120 * time.Second),
		},
	})
	if err != nil {
		return nil, err
	}

	broker := &Broker{
		container: created,
		URL:       fmt.Sprintf("amqp://%s:%s@127.0.0.1:%d/", brokerUser, brokerPass, port),
	}
	// An open port is not a loaded topology. Waiting for the queue is what
	// keeps a test from publishing into a half-declared broker and then
	// wondering where its message went — the same check Compose's healthcheck
	// makes, for the same reason.
	if err := broker.await(120*time.Second, true); err != nil {
		return nil, err
	}
	return broker, nil
}

// Stop kills the broker, as `docker stop togethergo-rabbitmq` would.
func (b *Broker) Stop() {
	b.t.Helper()
	timeout := 10 * time.Second
	require.NoError(b.t, b.container.Stop(context.Background(), &timeout), "stop rabbitmq container")
}

// Start brings it back on the same host port and waits until the topology is
// answering again.
func (b *Broker) Start() {
	b.t.Helper()
	require.NoError(b.t, b.container.Start(context.Background()), "start rabbitmq container")
	require.NoError(b.t, b.await(120*time.Second, true))
}

// await blocks until an AMQP connection succeeds and, when checkTopology is
// set, until the queues from definitions.json are there.
//
// Polled rather than slept on: a fixed sleep is either flaky or slow, and
// usually both. The topology probe is a passive queue declare — it creates
// nothing, which is the rule this whole system follows about who owns the
// topology, and it errors if the queue is missing.
func (b *Broker) await(timeout time.Duration, checkTopology bool) error {
	deadline := time.Now().Add(timeout)
	var lastErr error

	for time.Now().Before(deadline) {
		lastErr = func() error {
			conn, err := amqp.DialConfig(b.URL, amqp.Config{
				Dial:   amqp.DefaultDial(2 * time.Second),
				Locale: "en_US",
			})
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()

			if !checkTopology {
				return nil
			}
			ch, err := conn.Channel()
			if err != nil {
				return err
			}
			defer func() { _ = ch.Close() }()
			_, err = ch.QueueInspect(NotificationQueue)
			return err
		}()
		if lastErr == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("rabbitmq was not ready within %s: %w", timeout, lastErr)
}

// Consume drains up to `count` messages from a queue, or fails after timeout.
//
// Manual ack, one at a time, exactly as contracts/events.md requires of a real
// consumer — a test that acked in bulk would be testing a client nobody is
// allowed to write.
func (b *Broker) Consume(queue string, count int, timeout time.Duration) [][]byte {
	b.t.Helper()

	conn, err := amqp.DialConfig(b.URL, amqp.Config{
		Dial:   amqp.DefaultDial(5 * time.Second),
		Locale: "en_US",
	})
	require.NoError(b.t, err, "dial rabbitmq to consume")
	defer func() { _ = conn.Close() }()

	ch, err := conn.Channel()
	require.NoError(b.t, err)
	defer func() { _ = ch.Close() }()
	require.NoError(b.t, ch.Qos(10, 0, false))

	deliveries, err := ch.Consume(queue, "", false, false, false, false, nil)
	require.NoError(b.t, err, "consume from %s", queue)

	bodies := make([][]byte, 0, count)
	deadline := time.After(timeout)
	for len(bodies) < count {
		select {
		case delivery, ok := <-deliveries:
			if !ok {
				b.t.Fatalf("delivery channel closed after %d of %d messages", len(bodies), count)
			}
			bodies = append(bodies, delivery.Body)
			require.NoError(b.t, delivery.Ack(false))
		case <-deadline:
			b.t.Fatalf("timed out waiting for %d messages on %s; got %d", count, queue, len(bodies))
		}
	}
	return bodies
}

// Publish sends a raw body to the exchange under a routing key, the way
// another service's relay would.
//
// Raw bytes rather than a struct so a test can send a body that is not a valid
// envelope — which is one of the things the consumer has to have an answer for.
// Persistent and confirmed, exactly as a real publisher does it: an unconfirmed
// publish would make a test that never sees its message ambiguous between "the
// consumer ignored it" and "it was never on the queue".
func (b *Broker) Publish(routingKey string, body []byte) {
	b.t.Helper()

	conn, err := amqp.DialConfig(b.URL, amqp.Config{
		Dial:   amqp.DefaultDial(5 * time.Second),
		Locale: "en_US",
	})
	require.NoError(b.t, err, "dial rabbitmq to publish")
	defer func() { _ = conn.Close() }()

	ch, err := conn.Channel()
	require.NoError(b.t, err)
	defer func() { _ = ch.Close() }()
	require.NoError(b.t, ch.Confirm(false))

	confirm, err := ch.PublishWithDeferredConfirmWithContext(context.Background(),
		"togethergo.events", routingKey, false, false,
		amqp.Publishing{
			ContentType:     "application/json",
			ContentEncoding: "utf-8",
			DeliveryMode:    amqp.Persistent,
			Type:            routingKey,
			Body:            body,
		})
	require.NoError(b.t, err, "publish %s", routingKey)

	acked, err := confirm.WaitContext(context.Background())
	require.NoError(b.t, err)
	require.True(b.t, acked, "broker nacked a test publish of %s", routingKey)
}

// QueueDepth reports how many messages are sitting on a queue, ready and
// unacked-free. Used to assert that a message reached the DLQ, and that a
// well-handled one did not.
func (b *Broker) QueueDepth(queue string) int {
	b.t.Helper()

	conn, err := amqp.DialConfig(b.URL, amqp.Config{
		Dial:   amqp.DefaultDial(5 * time.Second),
		Locale: "en_US",
	})
	require.NoError(b.t, err)
	defer func() { _ = conn.Close() }()

	ch, err := conn.Channel()
	require.NoError(b.t, err)
	defer func() { _ = ch.Close() }()

	// A passive inspect: it declares nothing, which is the rule this whole
	// system follows about who owns the topology.
	state, err := ch.QueueInspect(queue)
	require.NoError(b.t, err, "inspect %s", queue)
	return state.Messages
}

// Purge empties a queue, so one test's events are not another's.
func (b *Broker) Purge(queue string) {
	b.t.Helper()

	conn, err := amqp.DialConfig(b.URL, amqp.Config{
		Dial:   amqp.DefaultDial(5 * time.Second),
		Locale: "en_US",
	})
	require.NoError(b.t, err)
	defer func() { _ = conn.Close() }()

	ch, err := conn.Channel()
	require.NoError(b.t, err)
	defer func() { _ = ch.Close() }()

	_, err = ch.QueuePurge(queue, false)
	require.NoError(b.t, err, "purge %s", queue)
}

// brokerConfigFiles locates the repository's own broker configuration.
//
// The container boots with the real deploy/rabbitmq files rather than with a
// topology the test invented, so the exchange, queues and bindings under test
// are the ones production uses. Found by walking up from the working directory
// looking for docker-compose.yml rather than by counting `..` segments, so
// moving a test between packages does not silently point it at nothing.
func brokerConfigFiles() (definitions, conf string, err error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", "", err
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err == nil {
			definitions = filepath.Join(dir, "deploy", "rabbitmq", "definitions.json")
			conf = filepath.Join(dir, "deploy", "rabbitmq", "rabbitmq.conf")
			for _, path := range []string{definitions, conf} {
				if _, err := os.Stat(path); err != nil {
					return "", "", fmt.Errorf("broker config %s: %w", path, err)
				}
			}
			return definitions, conf, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", errors.New("could not find the repository root (no docker-compose.yml above the working directory)")
		}
		dir = parent
	}
}

// freeBrokerPort asks the kernel for a port nobody is using and hands it back.
//
// Inherently racy — something could take it between the Close and the
// container's bind — but the alternative is a hard-coded port that collides
// with whatever `make up` left running, which fails far more often.
func freeBrokerPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}
