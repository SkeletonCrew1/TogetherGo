# TogetherGo web — production image.
#
# Two stages: node builds the bundle, nginx serves it. Nothing from the build
# stage but dist/ reaches the final image, so node, the toolchain and the
# dependency tree are not shipped.
#
# The development image is ./Dockerfile.dev, and that is what
# docker-compose.yml runs.

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM node:22-alpine AS build

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

# The two URLs the SPA talks to, as build arguments.
#
# ARG and not a runtime environment variable, and this is not a stylistic
# choice. Vite inlines every VITE_* value into the bundle at build time —
# `import.meta.env.VITE_API_BASE_URL` is replaced by a string literal in the
# emitted JavaScript. An image built with localhost baked in therefore cannot
# be repointed later by setting an env var on the container: the value is in
# the JavaScript the browser downloads, and nothing on the server side is ever
# consulted for it. Repointing means rebuilding with a different --build-arg.
#
# Both default to empty, which is what makes the bundle same-origin: the API
# client falls back to relative paths and the websocket derives its host from
# window.location. That is the right default when this image is served behind
# the same gateway as the API, which is the deployment this project has.
ARG VITE_API_BASE_URL=""
ARG VITE_WS_BASE_URL=""

# Promoted to ENV because Vite reads VITE_-prefixed variables off the process
# environment; an ARG alone is visible to RUN but not to the build.
ENV VITE_API_BASE_URL=$VITE_API_BASE_URL
ENV VITE_WS_BASE_URL=$VITE_WS_BASE_URL

COPY . .
RUN npm run build

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
FROM nginx:1.27-alpine AS runtime

# Replaces the default server block. The one thing it has to do that the
# default cannot is fall back to /index.html, so that a deep link like
# /my-trips survives a refresh instead of 404ing on a path that only exists in
# the router.
COPY nginx.conf /etc/nginx/conf.d/default.conf

COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 80

HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=5 \
    CMD wget -q -O /dev/null http://127.0.0.1/ || exit 1
