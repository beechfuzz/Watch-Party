# CLAUDE.md

Durable working rules for this repo. Keep this short — for the reasoning behind any non-obvious decision, read `ARCHITECTURE.md`, not here. For the full product spec and where the build diverged from it, read `SPEC.md`.

## Tech stack

- **Backend:** Go, stdlib `net/http` + `nhooyr.io/websocket`. No web framework.
- **Persistence:** SQLite via `modernc.org/sqlite` (pure Go, no CGO), migrated with `golang-migrate` (`internal/dbx/migrations`).
- **Frontend:** Server-rendered HTML + vanilla JavaScript. No framework, no bundler, no build step — files under `internal/webassets/web/static` are served as-is.
- **Container:** Multi-stage Dockerfile, static binary, distroless final stage (no shell). Target both Docker Compose and Podman quadlet; nothing Docker-socket-specific.

## Hard invariants

These must never be silently violated. If a change requires bending one, stop and flag it explicitly rather than working around it quietly.

- **The server is authoritative for playback state.** The host's client issues *requests* (play/pause/seek); it is never trusted as the source of truth. Every authoritative state change is validated, timestamped, sequenced, and broadcast by the party actor (`internal/party`), and every client — including the host's own — reacts to that broadcast, not to its local action.
- **Client-supplied Emby `PositionTicks` are never forwarded to Emby.** Any position reported to Emby (`Sessions/Playing*`) must be server-derived from the party's authoritative state (`syncalg.ExpectedPosition`), never taken verbatim from a client message. Client-reported position may be logged for diagnostics only.
- **Watch Party never proxies, caches, or transcodes media.** The server hands back an Emby-constructed playback URL; the participant's own browser streams directly from Emby using that participant's own token. No media bytes ever pass through this server.
- **Emby credentials, tokens, session cookies, and authenticated playback URLs are never logged — at any log level, including debug.** There is no automated redaction; this is enforced by not putting these values in a log call in the first place. Before adding a log statement near auth, sessions, or `emby.Client`, check what you're passing.
- **Media authorization is re-validated per participant, for whatever item is currently loaded — not inherited from the host, and not just checked once at join.** A party's current item can change after join (playlist advance, or a host selecting a different item), so the check re-fires every time it does, at the point each participant fetches a playback URL for it (`handlePlaybackURL`) — not only at the join-time gate, which only covers the item current *at that moment*. Being the host, or another member already having access, never implicitly grants anyone else access to an item they couldn't already see in Emby themselves.

## Where things live

```
cmd/server/main.go        Process entrypoint: config load, privdrop, DB open, wiring, graceful shutdown.
internal/config/          Env-var config loading and validation. No config files.
internal/dbx/             SQLite schema (migrations/), models, and the data access layer (store.go).
internal/session/         Opaque cookie sessions + synchronizer-token CSRF. No JWTs, no signing secret.
internal/emby/            Emby HTTP client: auth, item metadata, playback URL construction, progress reporting.
internal/party/           The sync engine: one actor goroutine per active party (party.go) + the registry (hub.go).
internal/wsproto/         The WebSocket wire protocol (message types, envelopes, payloads).
internal/httpapi/         HTTP handlers and routing — auth, party REST endpoints, the WS upgrade handler (ws.go).
internal/syncalg/         Pure, unit-tested drift/clock-offset/staleness math — canonical reference, mirrored in JS.
internal/embyreport/      Best-effort per-user Emby playback-progress reporting side channel.
internal/privdrop/        PUID/PGID privilege drop (root → unprivileged) for the distroless container.
internal/webassets/web/   Server-rendered templates + static frontend (JS mirrors internal/syncalg's math).
```

- **Sync hub:** `internal/party/hub.go` (registry, startup recovery, inactivity sweep) + `party.go` (per-party actor, one goroutine per party, all state mutation serialized through it).
- **Emby client:** `internal/emby/client.go` — every call takes the requesting user's own access token; nothing here accepts a shared/service token.
- **Session handling:** `internal/session/session.go` — cookie issuance/validation, CSRF token comparison.
- **Migrations:** `internal/dbx/migrations/*.up.sql` / `*.down.sql` — never edit a shipped migration; add a new one.

## Build, test, run

```
go run ./cmd/server                                          # needs env vars from .env.example set
go build ./...
go test ./...                                                 # unit + integration tests
go test -race ./...
node --test internal/webassets/web/static/js/*.test.mjs       # frontend JS tests: sync math (mirrors internal/syncalg), playlist selection logic, native-pause/ended event forwarding
docker compose up -d --build                                  # or: podman quadlet, see watchparty.container
```

Copy `.env.example` to `.env` first; `EMBY_SERVER_URL`, `APP_ORIGINS`, and `TOKEN_ENCRYPTION_KEY` (`openssl rand -base64 32`, or `./watchparty --generate-key`) are required. `internal/privdrop`'s tests that exercise real privilege-drop syscalls only run when `go test` itself runs as root; expect them skipped locally and in most CI.

## Further reading

`ARCHITECTURE.md` records every material decision (library choices, session design, WS protocol, sync algorithm, container privilege model, and a run of real-deployment bug postmortems) with alternatives considered. `SPEC.md` is the original project spec, annotated with where the build resolved open questions or diverged from it. Read `ARCHITECTURE.md` before re-deciding something that looks arbitrary — it probably isn't.
