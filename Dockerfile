# syntax=docker/dockerfile:1

# --- build stage -------------------------------------------------------
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO disabled: modernc.org/sqlite is a pure-Go SQLite driver, so the
# binary stays statically linked with no libc dependency — required for
# the distroless/static final stage below. Migrations and web assets are
# embedded into the binary via go:embed (see internal/dbx, internal/webassets),
# so there is nothing else to copy into the final image.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/watchparty ./cmd/server

# A starting point for the SQLite data directory, created here (as root)
# so it can be copied into the final stage with the nonroot user already
# owning it — distroless has no shell to chown at runtime.
RUN mkdir -p /out/data

# --- final stage ---------------------------------------------------------
# distroless/static: no shell, no package manager, minimal attack surface.
# Includes a CA bundle (needed for HTTPS calls to the Emby server) and runs
# as a non-root user by default via the :nonroot tag.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=build /out/watchparty /app/watchparty
COPY --from=build --chown=nonroot:nonroot /out/data /data

USER nonroot:nonroot
EXPOSE 8080

# No Docker-specific HEALTHCHECK instruction: this image is meant to run
# under Podman/podman-quadlet as well as Docker Compose, so orchestration
# should probe GET /healthz directly rather than relying on Docker-only
# healthcheck tooling (see docker-compose.yml and watchparty.container for
# the two ways of doing that).
ENTRYPOINT ["/app/watchparty"]
