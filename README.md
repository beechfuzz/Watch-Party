# Watch Party

An open-source, self-hosted, OCI-compliant companion app that lets Emby users watch the same media item in synchronized playback — like Overseerr/Jellyseerr, but for watching together instead of requesting media. Users sign in with their existing Emby credentials, create or join a "party," and watch in sync.

Watch Party **never downloads, caches, transcodes, or proxies media**. It only coordinates playback state; each participant's browser streams directly from your Emby server using that participant's own Emby token. See [`ARCHITECTURE.md`](ARCHITECTURE.md) for the full design rationale and every material decision made while building this.

## How it works, in one paragraph

You sign in with your Emby username/password (proxied straight to Emby — Watch Party never stores the password, only the resulting token, encrypted at rest). A host creates a party for a specific Emby item and shares the party link. Everyone who joins streams that item directly from your Emby server under their own account, while a WebSocket connection keeps everyone's playback in lockstep: the server holds the single authoritative "what position, playing or paused" state, and every client corrects toward it — gently nudging the playback rate for small drift, hard-seeking for large drift.

## Setup

### Prerequisites

- An Emby server, reachable (over HTTPS in production) from wherever Watch Party runs.
- An Emby user account for each person who'll use Watch Party — no special admin rights are needed; Watch Party only ever acts with a signed-in user's own permissions, never a shared service account.
- Docker or Podman.

### Required Emby permissions

Nothing special. Watch Party authenticates as whatever Emby user signs in and only ever uses that user's own access — if a user can already watch an item directly in Emby, they can watch it in a Watch Party party; if they can't, joining a party for that item is rejected the same way (see "Media authorization" below). There is no Watch Party-specific role or API key to configure on the Emby side — only the CORS configuration described next.

### CORS: the one thing you must configure on the Emby side

Because Watch Party runs on a different origin than Emby, and because a browser `<video>` element streams directly from Emby, **your browser will make cross-origin requests straight to Emby**. A shared parent domain (`watchparty.example.com` and `emby.example.com`) does **not** avoid this — same-site and same-origin are different things, and the browser enforces CORS based on origin, not site.

Watch Party authenticates these requests via an `api_key` query parameter (not a cookie), so the CORS policy you need is the simple, non-credentialed kind — you do **not** need `Access-Control-Allow-Credentials`. If Emby and Watch Party both sit behind the same Traefik instance, add a CORS headers middleware to Emby's router:

```yaml
# Traefik dynamic config (or equivalent labels), applied to Emby's router
http:
  middlewares:
    emby-cors:
      headers:
        accessControlAllowMethods: ["GET", "HEAD", "OPTIONS"]
        accessControlAllowOriginList: ["https://watchparty.example.com"]
        accessControlAllowHeaders: ["Range"]
        accessControlExposeHeaders: ["Content-Range", "Accept-Ranges", "Content-Length"]
        accessControlMaxAge: 3600
```

Or as Docker labels on the Emby container:

```yaml
labels:
  - "traefik.http.middlewares.emby-cors.headers.accesscontrolallowmethods=GET,HEAD,OPTIONS"
  - "traefik.http.middlewares.emby-cors.headers.accesscontrolalloworiginlist=https://watchparty.example.com"
  - "traefik.http.middlewares.emby-cors.headers.accesscontrolallowheaders=Range"
  - "traefik.http.middlewares.emby-cors.headers.accesscontrolexposeheaders=Content-Range,Accept-Ranges,Content-Length"
  - "traefik.http.routers.emby.middlewares=emby-cors"
```

**This was researched, not verified against a live Emby server** (see [`ARCHITECTURE.md` §0.4](ARCHITECTURE.md#04-cors--the-one-real-open-risk) for exactly what was and wasn't confirmed, and why). Before relying on this in production: load a party, open the browser console, and check for CORS errors on the `<video>` element's requests. If Emby's own defaults already permit this (some servers are more permissive out of the box), the Traefik middleware above is a no-op layer of defense-in-depth rather than a strict requirement — but confirm live rather than assuming either way.

### Running it

1. Copy `.env.example` to `.env` and fill in `EMBY_SERVER_URL`, `APP_ORIGINS`, and a generated `TOKEN_ENCRYPTION_KEY` (`openssl rand -base64 32`, or run the binary once with `--generate-key`). Review the other variables — the defaults are reasonable but the file documents what each one does.
2. `docker compose up -d --build` (see `docker-compose.yml`), or install `watchparty.container` under Podman Quadlet (see the comments in that file for install paths) and `systemctl start watchparty`.
3. Confirm `GET /healthz` returns 200 from whatever's checking container health — the image is distroless (no shell), so health must be checked externally against this endpoint rather than via a Docker/Podman exec-based healthcheck; see the comments in `docker-compose.yml` and `watchparty.container`.
4. Visit the app, sign in with an Emby account, and create a party for an Emby item.

### Local development

```
go run ./cmd/server   # needs the env vars from .env.example set; DEV_MODE=true permits http:// cookies
go test ./...
node --test internal/webassets/web/static/js/sync.test.mjs   # frontend sync-math tests
```

## Environment variables

Every variable Watch Party recognizes, with its default — all of these can be set the same way whether you're running via `docker run`/`docker compose`, `podman run`, or a Podman Quadlet `.container` unit (as plain `Environment=`/`EnvironmentFile=` entries — see the comments in `watchparty.container`). `.env.example` has the same list with longer inline explanations.

| Variable | Default | Purpose |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Address the HTTP server listens on. |
| `APP_ORIGINS` | *(required)* | Comma-separated origins this app is served from, e.g. `https://watchparty.example.com`. Enforced on WebSocket connections. |
| `DEV_MODE` | `false` | Permits non-Secure cookies for local `http://` testing only. Never enable in production. |
| `EMBY_SERVER_URL` | *(required)* | Your Emby server's base URL. |
| `PUID` | `65532` | UID the server process runs as — see [Running as a specific user](#running-as-a-specific-user-puidpgid) below. |
| `PGID` | `65532` | GID the server process runs as — same section. |
| `DATABASE_PATH` | `/data/watchparty.db` | Path to the SQLite database file; the directory is created (and chowned to `PUID:PGID`, if running as root) if missing. Point this somewhere writable instead, e.g. `./data/watchparty.db`, for local `go run` development. |
| `TOKEN_ENCRYPTION_KEY` | *(required)* | 32-byte key (base64 or hex) encrypting stored Emby tokens at rest. Never stored in SQLite. Generate with `openssl rand -base64 32` or `./watchparty --generate-key`. |
| `SESSION_IDLE_TIMEOUT` | `24h` | Sliding idle timeout — a session with no activity this long is invalidated. |
| `SESSION_MAX_AGE` | `720h` (30 days) | Absolute maximum session age, regardless of activity. |
| `HOST_GRACE_PERIOD_SECONDS` | `20` | How long a disconnected host has to reconnect before host status transfers. |
| `SYNC_SNAPSHOT_INTERVAL` | `4s` | How often the server broadcasts a full authoritative snapshot to every participant. |
| `SYNC_SOFT_DRIFT_MS` | `300` | Drift below this (milliseconds) is left uncorrected. |
| `SYNC_HARD_DRIFT_MS` | `1500` | Drift above this (milliseconds) triggers a hard seek. Must be greater than `SYNC_SOFT_DRIFT_MS`. |
| `SYNC_MAX_RATE_ADJUSTMENT` | `0.05` | Max fractional playback-rate nudge for gradual drift correction (`0.05` ⇒ rate range `[0.95, 1.05]`). |
| `EMBY_PROGRESS_INTERVAL` | `10s` | How often each participant's watch progress is reported back to their own Emby account. |
| `LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. |

`SESSION_IDLE_TIMEOUT`, `SESSION_MAX_AGE`, and `HOST_GRACE_PERIOD_SECONDS` accept either a bare integer (seconds) or a Go duration string (`30m`, `24h`).

### Running as a specific user (`PUID`/`PGID`)

If you've used a linuxserver.io-style image before, this works the same way: set `PUID`/`PGID` to the UID/GID you want the server to actually run as — typically whatever owns a host directory you're bind-mounting in, so files Watch Party creates aren't owned by some arbitrary container-internal ID that your host user can't touch.

The container image starts as root, but only ever as a brief, fixed first step: before opening its database or listening on anything, the server itself takes ownership of its data directory (`chown -R` to `PUID:PGID`) and then **permanently** drops root privileges down to `PUID:PGID` for the rest of the process's life — there's no shell or `su-exec`/`gosu` involved (the image is still distroless — no shell, no package manager), the Go binary does this itself via a raw syscall applied across every OS thread it's running on, which is the part that's easy to get subtly wrong (a naive single-thread privilege drop can leave *other* threads still running as root) and is covered by tests exercising the real syscalls, not just mocked logic. See `internal/privdrop` and `ARCHITECTURE.md` for the implementation and the reasoning.

If you don't set `PUID`/`PGID`, both default to `65532` — an arbitrary, unprivileged, non-root ID (the same one this image used to hardcode before `PUID`/`PGID` existed), so you get sane non-root behavior with zero configuration. Setting either to `0` is possible but not recommended — you'll get a startup log warning, since running the server as root is what this whole mechanism exists to avoid.

## Media authorization

Every participant streams under their own Emby token — never a shared one. Access is checked with **that specific user's** credentials both when they first view a party and again at the moment they actually join (over the WebSocket) — being the host, or another member already having access, never implicitly grants anyone else access to an item they couldn't already see in Emby.

## Party lifecycle and host transfer, in plain language

A party moves through `created → active → ended`, and `ended` is final — a party can never come back to life once ended. Only the host can play, pause, seek, transfer host status, or end the party; those actions are enforced on the server, not just hidden in the UI, so a participant can't bypass them by talking to the API directly.

If the host's connection drops, the party doesn't end and host status doesn't move immediately — a disconnected host has `HOST_GRACE_PERIOD_SECONDS` (default 20s) to reconnect and keep host status, since a network hiccup or a backgrounded tab shouldn't cost you control of the party. If that window passes without the host reconnecting, host status moves automatically to whichever connected participant has been in the party the longest. Once that transfer happens, the original host doesn't automatically get host status back just by reconnecting afterward — the party would otherwise flap back and forth if the original host's connection was flaky. If the host explicitly leaves (rather than just disconnecting), the transfer happens immediately, without waiting out the grace period, since that's a deliberate action rather than a network blip.

A dropped connection and someone actually leaving are tracked separately: `disconnected` participants keep their place in the party (and their position in the host-succession order) and pick back up on reconnect; only an explicit "leave" (or the party ending) removes someone for good.

The party itself — not any one person's browser — decides when playback has reached the end. Every few seconds the server checks the current position against the item's actual duration (fetched from Emby when the party was created); if it's past the end, the server pauses everyone at the end and reports that to each participant's own Emby watch history. A single participant's browser reporting "this video ended" (which can happen spuriously, e.g. on a stall) never unilaterally stops playback for everyone else — there isn't even a wire message for it.

## Drift correction, in plain language

The server is the single source of truth for "where should everyone be right now." Every client periodically compares where its own video actually is against where the server says it should be. Small differences (a few hundred milliseconds, configurable) are corrected by very slightly speeding up or slowing down playback — imperceptible, and it smoothly returns to normal speed once caught up, rather than staying slightly off-speed forever. Larger differences (over a second or so, configurable) are corrected with a direct seek instead, since nudging the rate would take too long to catch up and a slightly visible jump is better than staying out of sync for a while.

Comparing "where should everyone be right now" fairly across different people's computers requires knowing how far off each computer's clock is from the server's — that's what the periodic clock-sync handshake measures, using round-trip timing rather than assuming each participant's one-way network lag is exactly half their round-trip time (it usually isn't, quite).

New or reconnecting participants never try to replay everything that happened while they were away — they just get a fresh, complete snapshot of the current state and sync straight to that.

## Testing

- `go test ./...` — unit tests (drift calculation, clock-offset estimation, sequence-number ordering/staleness rejection, host authorization, host-transfer selection, grace-period behavior, the `PUID`/`PGID` privilege-drop syscalls) and integration tests against the real party actor (host/participant connect, play/seek/pause propagation, disconnect/reconnect with a correct snapshot, rapid-succession command ordering, end-of-media detection, explicit and grace-period host transfer). The `internal/privdrop` tests that exercise the real privilege-drop syscalls only run when `go test` itself is run as root (they `t.Skip` otherwise) — CI runners typically aren't root, so don't be surprised to see them skipped there.
- `node --test internal/webassets/web/static/js/sync.test.mjs` — the same drift/clock-offset/staleness math, mirrored in the browser client (see `ARCHITECTURE.md` §1.6 for why it's mirrored rather than shared), tested the same way.

## License

Add a license of your choice before publishing this publicly — none is specified yet.
