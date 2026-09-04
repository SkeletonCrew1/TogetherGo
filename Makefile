# TogetherGo — local development.
#
# Everything here drives docker compose against the infrastructure defined in
# docker-compose.yml. Run `make help` for the target list.

COMPOSE      := docker compose
DEFINITIONS  := deploy/rabbitmq/definitions.json
KEYS_DIR     := deploy/keys
PRIVATE_KEY  := $(KEYS_DIR)/jwt_private.pem
PUBLIC_KEY   := $(KEYS_DIR)/jwt_public.pem

# Loaded so the psql-* targets can use the superuser credentials.
ifneq (,$(wildcard .env))
include .env
export
endif

.DEFAULT_GOAL := help

.PHONY: help up down logs ps restart rebuild clean keys \
        psql-identity psql-trip psql-chat psql-notification \
        rabbitmq-ui rabbitmq-topology rabbitmq-import mailhog-ui check-env \
        migrate migrate-identity migrate-trip migrate-trip-down \
        migrate-chat migrate-chat-down \
        test-identity test-trip test-trip-race test-chat test-chat-race \
        lint-trip lint-chat openapi-identity scale-chat

## help: list available targets
help:
	 @grep -hE "^## " $(firstword $(MAKEFILE_LIST)) | sed 's/^## /  /' | sort

check-env:
	@test -f .env || { \
		echo "error: .env is missing. Run: cp .env.example .env"; \
		exit 1; \
	}
	@# The broker gets its credentials from definitions.json, clients get theirs
	@# from .env. Drift between the two shows up as an opaque auth failure at
	@# connect time, so catch it here instead.
	@python3 -c "import json,sys; \
		d=json.load(open('$(DEFINITIONS)')); \
		u=[x for x in d['users'] if x['name']=='$(RABBITMQ_USER)']; \
		sys.exit(0) if u and u[0].get('password')=='$(RABBITMQ_PASSWORD)' else \
		sys.exit('error: RABBITMQ_USER/RABBITMQ_PASSWORD in .env do not match the user in $(DEFINITIONS)')"

## up: start the infrastructure and wait for every container to be healthy
up: check-env
	$(COMPOSE) up -d --wait

## down: stop and remove containers, keeping volumes
down:
	$(COMPOSE) down --remove-orphans

## restart: recreate every container
restart: down up

## rebuild: rebuild the built images and restart everything (make rebuild SERVICE=trip)
rebuild: check-env
	@# `up` alone reuses whatever image is already tagged, so a code change is
	@# invisible to it. This is the target to run after editing a service.
	$(COMPOSE) up -d --build --wait $(SERVICE)
	@# The gateway's routing table is a bind-mounted file, not part of any
	@# image, so it is never rebuilt — but on Docker Desktop for macOS the
	@# inotify event does not reliably cross the bind mount, and an edit to
	@# deploy/traefik/dynamic.yml can sit there unread. Restarting is cheap and
	@# makes "did my route land" a question with one answer.
	$(COMPOSE) restart gateway
	@$(COMPOSE) ps

## ps: show container status and health
ps:
	$(COMPOSE) ps

## logs: follow logs from every container (make logs SERVICE=postgres to narrow)
logs:
	$(COMPOSE) logs -f --tail=100 $(SERVICE)

## clean: stop everything and delete volumes (destroys all local data)
clean:
	$(COMPOSE) down --volumes --remove-orphans
	@echo "volumes removed; the next 'make up' re-runs deploy/postgres/init/"

## keys: generate the RS256 keypair used by the identity service
keys:
	@mkdir -p $(KEYS_DIR)
	@if [ -f $(PRIVATE_KEY) ]; then \
		echo "$(PRIVATE_KEY) already exists; delete it first to rotate"; \
	else \
		openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 \
			-out $(PRIVATE_KEY) 2>/dev/null; \
		openssl rsa -in $(PRIVATE_KEY) -pubout -out $(PUBLIC_KEY) 2>/dev/null; \
		chmod 600 $(PRIVATE_KEY); \
		chmod 644 $(PUBLIC_KEY); \
		echo "wrote $(PRIVATE_KEY) and $(PUBLIC_KEY)"; \
	fi

## migrate: run every service's migrations
migrate: migrate-identity migrate-trip migrate-chat

## migrate-identity: apply identity_db migrations (never runs on startup)
migrate-identity:
	$(COMPOSE) run --rm --no-deps identity alembic upgrade head

## migrate-trip: apply trip_db migrations (never runs on startup)
migrate-trip:
	$(COMPOSE) run --rm --no-deps trip migrate up

## migrate-trip-down: roll back the most recent trip_db migration
migrate-trip-down:
	$(COMPOSE) run --rm --no-deps trip migrate down

## migrate-chat: apply chat_db migrations (never runs on startup)
migrate-chat:
	$(COMPOSE) run --rm --no-deps chat migrate up

## migrate-chat-down: roll back the most recent chat_db migration
migrate-chat-down:
	$(COMPOSE) run --rm --no-deps chat migrate down

## test-identity: run the identity test suite (needs a working docker socket)
test-identity:
	cd services/identity && uv run pytest -q

## test-trip: run the trip test suite (starts postgis and rabbitmq containers)
test-trip:
	cd services/trip && go test ./...

## test-trip-race: the capacity invariant under -race, ten times over
test-trip-race:
	@# The acceptance criterion for join-request approval. Ten runs because a
	@# lock bug that shows up one time in three would pass a single run often
	@# enough to be believed.
	cd services/trip && go test ./internal/store/ -run TestConcurrent -race -count=10

## test-chat: run the chat test suite (starts postgres, redis and rabbitmq containers)
test-chat:
	cd services/chat && go test ./...

## test-chat-race: the socket and fan-out tests under -race
test-chat-race:
	@# The acceptance criteria for this service are about concurrency: one hub
	@# serving many sockets, two goroutines per connection, and a fan-out
	@# goroutine writing to both. A data race here would show up as a message
	@# delivered to the wrong room, which no functional test would catch.
	cd services/chat && go test ./internal/http/ ./internal/realtime/ -race -count=1

## lint-trip: vet and format-check the trip service
lint-trip:
	cd services/trip && go vet ./...
	@cd services/trip && unformatted="$$(gofmt -l .)"; \
		test -z "$$unformatted" || { \
			echo "gofmt found unformatted files:"; echo "$$unformatted"; exit 1; \
		}

## lint-chat: vet and format-check the chat service
lint-chat:
	cd services/chat && go vet ./...
	@cd services/chat && unformatted="$$(gofmt -l .)"; \
		test -z "$$unformatted" || { \
			echo "gofmt found unformatted files:"; echo "$$unformatted"; exit 1; \
		}

## scale-chat: run two chat replicas behind the gateway (make scale-chat N=3)
scale-chat: check-env
	@# The acceptance criterion for the Redis fan-out. Two replicas share one
	@# database and one Redis; a message written on either has to reach the
	@# sockets held by the other, and the gateway load-balances between them
	@# with no sticky sessions.
	$(COMPOSE) up -d --wait --scale chat=$(or $(N),2) chat
	@$(COMPOSE) ps chat

## openapi-identity: regenerate contracts/openapi/identity.yaml from the app
openapi-identity:
	cd services/identity && uv run python -m app.openapi ../../contracts/openapi/identity.yaml

## psql-identity: open psql on identity_db as identity_user
psql-identity:
	$(COMPOSE) exec -e PGPASSWORD=$(IDENTITY_DB_PASSWORD) postgres \
		psql -U identity_user -d identity_db

## psql-trip: open psql on trip_db as trip_user
psql-trip:
	$(COMPOSE) exec -e PGPASSWORD=$(TRIP_DB_PASSWORD) postgres \
		psql -U trip_user -d trip_db

## psql-chat: open psql on chat_db as chat_user
psql-chat:
	$(COMPOSE) exec -e PGPASSWORD=$(CHAT_DB_PASSWORD) postgres \
		psql -U chat_user -d chat_db

## psql-notification: open psql on notification_db as notification_user
psql-notification:
	$(COMPOSE) exec -e PGPASSWORD=$(NOTIFICATION_DB_PASSWORD) postgres \
		psql -U notification_user -d notification_db

## rabbitmq-ui: open the RabbitMQ management UI
rabbitmq-ui:
	@echo "http://localhost:$(or $(RABBITMQ_MANAGEMENT_PORT),15672)  (user: $(or $(RABBITMQ_USER),togethergo))"
	@open http://localhost:$(or $(RABBITMQ_MANAGEMENT_PORT),15672) 2>/dev/null \
		|| xdg-open http://localhost:$(or $(RABBITMQ_MANAGEMENT_PORT),15672) 2>/dev/null \
		|| true

## rabbitmq-topology: print the declared exchanges, queues and bindings
rabbitmq-topology:
	@echo "== exchanges =="
	@$(COMPOSE) exec -T rabbitmq rabbitmqctl -q list_exchanges name type durable
	@echo
	@echo "== queues =="
	@$(COMPOSE) exec -T rabbitmq rabbitmqctl -q list_queues name durable arguments
	@echo
	@echo "== bindings =="
	@$(COMPOSE) exec -T rabbitmq rabbitmqctl -q list_bindings \
		source_name routing_key destination_name

## rabbitmq-import: re-apply deploy/rabbitmq/definitions.json to a running broker
rabbitmq-import:
	$(COMPOSE) exec -T rabbitmq rabbitmqctl import_definitions \
		/etc/rabbitmq/definitions.json
	@echo "imported; note that import only adds, it never deletes"

## mailhog-ui: open the MailHog web UI
mailhog-ui:
	@echo "http://localhost:$(or $(MAILHOG_UI_PORT),8025)"
	@open http://localhost:$(or $(MAILHOG_UI_PORT),8025) 2>/dev/null \
		|| xdg-open http://localhost:$(or $(MAILHOG_UI_PORT),8025) 2>/dev/null \
		|| true
