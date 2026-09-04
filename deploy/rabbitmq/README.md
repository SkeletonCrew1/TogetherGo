# RabbitMQ topology

`definitions.json` is the single source of truth for exchanges, queues and
bindings. It is imported by the broker at boot (`load_definitions` in
`rabbitmq.conf`), so the topology exists before any service connects and no
service declares it. Publishers publish to `togethergo.events`; consumers
`basic.consume` from a queue that is already there.

The contract these files implement is documented in
[`contracts/events.md`](../../contracts/events.md) — read that first; this
directory is only its machine-readable form.

## What is declared

| Exchange | Type | Purpose |
| --- | --- | --- |
| `togethergo.events` | topic, durable | every domain event, routing key = `event_type` |
| `togethergo.dlx` | topic, durable | dead letters, routing key = the originating queue's name |

| Queue | Owner | Bindings on `togethergo.events` |
| --- | --- | --- |
| `notification.events` | notification | `user.registered`, `trip.status_changed`, `trip.completed`, `trip.cancelled`, `trip.invite_sent`, `join_request.*` |
| `chat.trip-events` | chat | `trip.created`, `trip.cancelled`, `join_request.approved`, `participant.removed` |
| `identity.trip-events` | identity | `trip.completed` |
| `trip.user-events` | trip | `user.profile_updated`, `user.rating_updated` |

Every queue above is durable and carries
`x-dead-letter-exchange: togethergo.dlx` plus
`x-dead-letter-routing-key: <its own name>`. Each has a `<queue>.dlq` bound to
`togethergo.dlx` with that same routing key, so a rejected message lands in the
DLQ of the queue it failed on and nowhere else. DLQs have no dead-letter
arguments of their own — a dead letter is a terminus, not a loop.

## Changing the topology

1. Edit `definitions.json`.
2. `make rabbitmq-import` to apply it to a running broker, or `make restart`.

Import never deletes. Removing a queue or binding from the file does not remove
it from a broker that already has it — delete it by hand in the management UI
or `rabbitmqctl`, or run `make clean` to drop the volume and start over.

## Verifying

```
make rabbitmq-topology   # prints exchanges, queues and bindings
make rabbitmq-ui         # http://localhost:15672
```

## Credentials and the vhost

`definitions.json` also declares the broker's single user and its permissions.
That is not decoration: a node that has definitions to load logs

```
Will not seed default virtual host and user: have definitions to load...
```

and skips default-user seeding, so `RABBITMQ_DEFAULT_USER` and
`RABBITMQ_DEFAULT_PASS` have no effect and the broker would come up with no
users at all. The user has to be in the file.

`RABBITMQ_USER` / `RABBITMQ_PASSWORD` in `.env` are the client half of the same
credentials and must match the user here — `make up` runs `check-env`, which
fails if they drift. These are local-development credentials; a deployed
environment gets its definitions rendered with real ones.

Everything is declared on the default vhost `/`. `RABBITMQ_VHOST` in `.env`
must stay `/` unless you also change every `"vhost"` in `definitions.json`.
