# syntax=docker/dockerfile:1

# --- build stage -------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO disabled: modernc.org/sqlite is a pure-Go SQLite driver, so the
# binary stays statically linked with no libc dependency — required for
# the distroless/static final stage below. It's also required for
# internal/privdrop's use of syscall.AllThreadsSyscall, which always
# returns ENOTSUP in cgo binaries. Migrations and web assets are embedded
# into the binary via go:embed (see internal/dbx, internal/webassets), so
# there is nothing else to copy into the final image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/watchparty ./cmd/server

# --- final stage ---------------------------------------------------------
# distroless/static: no shell, no package manager, minimal attack surface.
# Includes a CA bundle (needed for HTTPS calls to the Emby server).
#
# Deliberately the root-starting variant (no :nonroot tag, no USER
# directive): the process needs to start as root so it can take ownership
# of a possibly root-owned or foreign-UID-owned bind-mounted /data volume
# for an operator-chosen PUID/PGID, matching the common self-hosted-image
# pattern. It permanently drops to PUID:PGID (defaulting to 65532:65532,
# the same UID this image used to hardcode via :nonroot) before opening
# the database or listening on anything — see internal/privdrop and
# ARCHITECTURE.md for why this is safe and how it's implemented without a
# shell or su-exec/gosu, neither of which distroless has room for.
FROM gcr.io/distroless/static-debian12
WORKDIR /app

COPY --from=build /out/watchparty /app/watchparty
EXPOSE 8080

# No Docker-specific HEALTHCHECK instruction: this image is meant to run
# under Podman/podman-quadlet as well as Docker Compose, so orchestration
# should probe GET /healthz directly rather than relying on Docker-only
# healthcheck tooling (see docker-compose.yml and watchparty.container for
# the two ways of doing that).
ENTRYPOINT ["/app/watchparty"]
