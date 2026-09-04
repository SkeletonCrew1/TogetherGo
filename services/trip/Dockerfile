# TogetherGo trip service.
#
# Two stages. The dependency layer is keyed on go.mod + go.sum alone, so editing
# application code does not redownload the module cache. The runtime image
# carries two static binaries and nothing else — no toolchain, no source, no
# .sql files (the migrations are embedded in the migrate binary).

FROM golang:1.23-alpine AS builder

WORKDIR /src

# Cached separately from the source: these two files change far less often.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off because nothing here needs it — pgx is pure Go — and a static binary
# is what makes the tiny runtime image possible.
#
# -trimpath keeps build-machine paths out of panic traces; -s -w drops the
# symbol table and DWARF, which is a few megabytes of nothing useful in a
# container.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
    && go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate


FROM alpine:3.20

# wget for the healthcheck, ca-certificates so an https JWKS URL would work.
RUN apk add --no-cache ca-certificates wget

# Runs unprivileged: the service talks to Postgres and to identity over the
# network and reads nothing from disk.
RUN addgroup --system --gid 1001 trip \
    && adduser --system --uid 1001 --ingroup trip --no-create-home trip

COPY --from=builder /out/server /usr/local/bin/server
COPY --from=builder /out/migrate /usr/local/bin/migrate

USER trip

EXPOSE 8002

# Liveness only. /readyz is the gateway's business, and a readiness failure —
# identity being down, say — must not get this container killed.
# A plain GET rather than `--spider`, whose HEAD request would exercise a
# different route. Not quiet, so a failing probe leaves something in
# `docker inspect`'s health log rather than an empty string.
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=5 \
    CMD wget -O /dev/null http://127.0.0.1:8002/healthz || exit 1

# Migrations are never run on startup (CLAUDE.md): `make migrate-trip`.
CMD ["server"]
