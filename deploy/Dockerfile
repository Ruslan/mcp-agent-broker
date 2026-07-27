# Agent Task Broker — production image (Kamal 2).
#
# Three stages, because the admin UI is `go:embed`ed into the binary: the Svelte
# bundle must exist at agent-broker/dist/ BEFORE `go build` runs, and that dir is
# gitignored (built, never committed). So we build it here rather than relying on
# whatever happens to be on the build host.
#
# The binary is CGO-free: the store uses modernc.org/sqlite (pure Go), so there is
# no libsqlite3 to link and the result runs on a bare alpine.

# --- 1. admin UI (Svelte → dist/) --------------------------------------------
FROM node:22-alpine AS ui
WORKDIR /ui
# package.json + lockfile first: this layer re-uses the npm cache unless deps change.
COPY ui/package.json ui/package-lock.json* ./
RUN npm ci
COPY ui/ ./
RUN npm run build

# --- 2. Go binary ------------------------------------------------------------
FROM golang:1.25 AS build
WORKDIR /src
COPY agent-broker/go.mod agent-broker/go.sum ./
RUN go mod download
COPY agent-broker/ ./
# The embed target (see admin_embed.go). Copied from stage 1, not from the host.
COPY --from=ui /ui/dist ./dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/broker .

# --- 3. runtime --------------------------------------------------------------
FROM alpine:3.20
# ca-certificates: outbound HTTPS. sqlite/curl: ops tooling — inspecting the prod DB
# and probing endpoints from INSIDE the container (`make kamal sql`, `make kamal shell`)
# without spinning a helper container. While the project is in active development,
# adding diagnostic tooling here is fine: it is one apk line and a cached rebuild.
RUN apk add --no-cache ca-certificates sqlite curl
WORKDIR /app
COPY --from=build /out/broker /usr/local/bin/broker
# Role prompts are read from disk at boot (PROMPTS_DIR), not embedded.
COPY prompts/ /app/prompts/
# Defaults for the container; deploy-time env may override. DB_PATH points at the
# Kamal volume so the database survives image rebuilds — see config/deploy.yml.
ENV PORT=9197 \
    DB_PATH=/data/broker.db \
    PROMPTS_DIR=/app/prompts
EXPOSE 9197
ENTRYPOINT ["broker"]
