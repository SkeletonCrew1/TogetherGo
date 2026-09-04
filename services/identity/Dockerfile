# TogetherGo identity service.
#
# Two stages so the runtime image carries no build tooling and no uv. The
# dependency layer is keyed on pyproject.toml + uv.lock alone, so editing
# application code does not reinstall the world.

FROM python:3.12-slim-bookworm AS builder

COPY --from=ghcr.io/astral-sh/uv:0.6.10 /uv /bin/uv

ENV UV_COMPILE_BYTECODE=1 \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    UV_PROJECT_ENVIRONMENT=/opt/venv

WORKDIR /srv

COPY pyproject.toml uv.lock ./
# --frozen: the lockfile is the contract. A build that would have to resolve
# differently should fail here rather than ship something untested.
RUN uv sync --frozen --no-dev


FROM python:3.12-slim-bookworm

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PATH="/opt/venv/bin:$PATH"

# Runs unprivileged: the service needs to read one key file and talk to the
# network, nothing that wants root.
RUN groupadd --system --gid 1001 identity \
    && useradd --system --uid 1001 --gid identity --no-create-home identity

COPY --from=builder /opt/venv /opt/venv

WORKDIR /srv
COPY --chown=identity:identity alembic.ini ./
COPY --chown=identity:identity migrations ./migrations
COPY --chown=identity:identity app ./app

USER identity

EXPOSE 8001

# Liveness only — /readyz is the gateway's business, and a readiness failure
# should not get the container killed.
HEALTHCHECK --interval=15s --timeout=5s --start-period=15s --retries=5 \
    CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8001/healthz', timeout=3).status == 200 else 1)"

# Migrations are never run on startup (CLAUDE.md): `make migrate-identity`.
# --no-access-log: the request-context middleware already logs one
# structured line per request, and uvicorn's own line would duplicate it in a
# different format. No --proxy-headers either: the service reads
# X-Forwarded-For itself where it needs it (see app/deps.py) rather than
# letting uvicorn rewrite request.client from a header any direct caller
# could forge.
CMD ["uvicorn", "app.main:create_app", "--factory", \
     "--host", "0.0.0.0", "--port", "8001", "--no-access-log"]
