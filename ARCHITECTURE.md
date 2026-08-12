# Architecture Decisions

This document records material architectural decisions made while building Watch Party, the alternatives considered, and why each was chosen. It is written incrementally, in build order, starting with the Emby investigation the project spec required before any Emby integration code was written.

---

## 0. Emby API investigation (Phase 0)

**Method.** The project spec calls for a live, throwaway proof-of-concept against a real Emby server before writing any integration code. The development sandbox this project was built in has an egress allowlist that does not include the operator's Emby server, so a live POC could not be run from here. With the operator's agreement, this investigation was done instead by researching Emby's own wiki documentation (`github.com/MediaBrowser/Emby/wiki`) and, for anything the wiki doesn't spell out at the wire-protocol level, cross-checking against Jellyfin's server source. Jellyfin was forked from Emby's last open-source snapshot in 2018 and still carries an explicit "legacy Emby" compatibility branch in its auth layer (`AuthorizationContext.cs` literally parses an `"Emby ..."` authorization scheme, and Jellyfin's internal namespace is still `Emby.Server.Implementations` in places) — so it's a reasonable corroborating source for the parts of the API that predate the fork, but **it is Jellyfin source, not Emby source**, and every finding below is marked with how it was confirmed.

**This is real risk, not a formality: treat every endpoint shape below as unverified until it's been exercised against the operator's actual Emby server**, and re-confirm before relying on any of this in production. The one item that could not be reduced to \"high confidence\" at all — CORS — is called out specifically in §0.4 with a concrete first thing to test.

### 0.1 Authentication — `POST /Users/AuthenticateByName`

- Requires an `X-Emby-Authorization` header identifying the client even for this call: `X-Emby-Authorization: Emby Client="Watch Party", Device="<device name>", DeviceId="<stable uuid>", Version="<app version>"`. Confirmed via the Emby wiki's User Authentication page and, at the code level, Jellyfin's `AuthorizationContext.cs` (these fields are copied verbatim onto the resulting session).
- Body: `{"Username": "...", "Pw": "..."}` — plaintext password, field name `Pw` not `Password`. (An old wiki revision described legacy SHA1/MD5 password-hash fields; those were retired well before this project's timeframe and are not used here.)
- Response: `{"User": {...}, "SessionInfo": {...}, "AccessToken": "...", "ServerId": "..."}`. Confirmed at the code level via Jellyfin's `AuthenticationResult` model, corroborated by the wiki's description of the response.
- The `AccessToken` is long-lived and reusable (not single-request scoped) — used on every subsequent call until logout or server-side revocation, at which point Emby returns 401 and the client must re-authenticate. This lines up with the spec's requirement to invalidate the local session (forcing reauthentication) rather than silently retrying a dead token.
- Token is sent on subsequent requests either as an `X-Emby-Token` header, or as an `api_key` (or `ApiKey`) query parameter. Both are accepted; confirmed via the wiki's API Key Authentication page and Jellyfin's auth parsing code.

**Decision:** Watch Party proxies the raw credentials directly to this endpoint, discards the plaintext password immediately after the call returns, and stores only the returned `AccessToken` (encrypted at rest — see §1.3). Confidence: high.

### 0.2 Playback URL mechanism — the crux of the "no proxy" design

**`GET/POST /Items/{itemId}/PlaybackInfo`** — call this first for any item about to be played. Response is `{"MediaSources": [...], "PlaySessionId": "...", "ErrorCode": ...}`. Each `MediaSourceInfo` entry has `Id`, `Container`, `SupportsDirectPlay`, `SupportsDirectStream`, `SupportsTranscoding`, `TranscodingUrl` (only populated for the transcode case), among others — **there is no `DirectStreamUrl` field**; for direct play/stream the client is expected to construct the URL itself. Confirmed at the code level via Jellyfin's `MediaSourceInfo` model; consistent with the Emby wiki, which also documents manual URL construction rather than a server-provided direct-stream URL.

**Direct Play / Direct Stream URL** (client-constructed):
```
GET /Videos/{itemId}/stream[.{container}]?Static=true&MediaSourceId={mediaSourceId}&PlaySessionId={playSessionId}&api_key={AccessToken}
```
`Static=true` tells Emby to serve the source file as-is instead of transcoding. The `api_key` query parameter is what makes this usable from a plain HTML `<video src>` — a video element cannot set custom headers, so query-string auth is the only viable mechanism for this specific request. Confirmed via the wiki's Video Streaming page; query-param auth confirmed at the code level via Jellyfin's `AuthorizationContext`.

**Transcoded / HLS URL** (client-constructed):
```
GET /Videos/{itemId}/master.m3u8?MediaSourceId={mediaSourceId}&PlaySessionId={playSessionId}&DeviceId={deviceId}&api_key={AccessToken}
```
Confirmed at the code level via Jellyfin's `DynamicHlsController`. Segment and key URLs emitted inside the playlist are relative and are expected to inherit the same query-string auth the playlist request carried — this specific detail is **not explicitly documented anywhere reachable during this investigation** and is rated medium confidence / inferred from how every Emby/Jellyfin web client actually behaves in practice, not from a citable spec. **Verify this first** against the operator's server: request `master.m3u8` with a valid `api_key`, and confirm the segment URLs inside the returned playlist are directly fetchable without additional auth.

After playback ends, `DELETE /Videos/ActiveEncodings?DeviceId={deviceId}` releases the server-side transcode job.

**`PlaySessionId`**: prefer the one returned by `PlaybackInfo`, since the server uses it to track and later cancel transcode jobs. The wiki allows a client-generated random string as a fallback, but there's no reason to use the fallback path here since Watch Party always calls `PlaybackInfo` first.

**Range requests:** confirmed supported on the static/direct-stream endpoint — Jellyfin serves it via `PhysicalFileResult { EnableRangeProcessing = true }`, the standard ASP.NET Core mechanism for `Range:`/`206 Partial Content` handling, which is exactly what a `<video>` element needs for native seeking. Rated high confidence for Emby by inference (Emby's server is also ASP.NET Core-based and shares direct lineage with this part of Jellyfin's stack) but not directly confirmed against Emby's closed-source binary.

**Decision:** the browser's `<video>` element is pointed directly at the constructed stream URL (Direct Play/Stream case) or handed to an HLS player against the `master.m3u8` URL (transcode case), both carrying the requesting user's own `api_key`. Watch Party's server never touches the media bytes — consistent with the spec's non-negotiable "no proxy" requirement. Confidence: high for the URL shapes and auth mechanism; medium for HLS segment auth inheritance (flagged above for live verification).

### 0.3 Per-user Emby playback reporting

`POST /Sessions/Playing`, `POST /Sessions/Playing/Progress`, `POST /Sessions/Playing/Stopped` all take the same JSON body shape: `ItemId`, `MediaSourceId`, `PositionTicks`, `IsPaused`, `PlayMethod` (`"DirectPlay" | "DirectStream" | "Transcode"`), `PlaySessionId`, among other optional fields. Confirmed via the Emby wiki's Playback Check-ins page, independently corroborated by Jellyfin's `PlaystateController` source (field-for-field match, strong corroboration since this endpoint predates the 2018 fork). Auth is the ordinary `X-Emby-Token` header — these are server-to-server-style calls made from Watch Party's backend using each participant's own token, not constrained by the `<video>` tag's inability to set headers. Confidence: high.

### 0.4 CORS — the one real open risk

Emby's own documentation site (`dev.emby.media`) and community forums were unreachable from the research environment (network policy), so **this is the one area of this investigation that is not confirmed against any Emby-authored source** — only forum thread *titles* suggesting other operators have hit CORS issues in front of Emby and worked around them via reverse-proxy configuration (nginx), and a code-level look at Jellyfin's CORS handling (which defaults to wildcard `Access-Control-Allow-Origin: *` unless the admin configures specific allowed hosts — a Jellyfin behavior, not a confirmed Emby one).

One relevant data point: an existing open-source project solving the same problem (`Oratorian/emby-watchparty`) chose to fully proxy Emby traffic through its own backend specifically to avoid browsers talking to Emby directly at all. That is a deliberate rejection of the architecture this spec requires, made by another team building the same kind of tool — worth taking seriously as a signal, but per the spec's explicit instruction, Watch Party is not adopting a media-proxying architecture; the operator has already accepted that trade-off and stated the reasoning (Emby's own uptime shouldn't become Watch Party's problem, and reimplementing Range/HLS-manifest passthrough is exactly the complexity this project is trying to avoid).

**Decision:** proceed with direct browser-to-Emby streaming as specified, but treat CORS as an explicit deployment prerequisite rather than an assumption:
- Because Watch Party authenticates Emby requests via the `api_key` query parameter (not cookies), the browser does not need to send credentials cross-origin for media requests — so the required CORS policy is the simple, non-credentialed kind (`Access-Control-Allow-Origin` naming the Watch Party origin, or `*`), not the more complex `Access-Control-Allow-Credentials: true` case that trips up cookie-based setups.
- The README documents the concrete Traefik middleware configuration needed to add these headers in front of Emby, since Emby's own CORS support could not be confirmed as sufficient (or even present) from this investigation.
- **First thing to test once the operator has live access:** load the constructed Direct Play URL in a `<video>` tag served from the Watch Party origin and check the browser console for CORS errors, before building anything else on top of it. If Emby's default behavior turns out to already be permissive (wildcard, like Jellyfin's default), the Traefik middleware becomes a no-op layer of defense-in-depth rather than a hard requirement — but that can only be confirmed live.

---

## 1. Library and implementation choices

The spec leaves several implementation details to be decided at build time, explicitly stating they don't materially affect architecture. Decisions:

### 1.1 WebSocket library: `nhooyr.io/websocket`
Chosen over `gorilla/websocket` (both were offered as options). `nhooyr.io/websocket` has a smaller API surface, is context-aware throughout (fits idiomatic use of `context.Context` for cancellation/deadlines alongside the rest of this codebase), and has no dependency on the older `x/net` websocket internals gorilla still carries. Gorilla is more widely deployed and would have been an equally reasonable choice; this was a coin-flip-with-a-slight-preference, not a decision with real stakes.

### 1.2 Migration tool: `golang-migrate/migrate` (not hand-rolled)
The spec allows either `golang-migrate` or a minimal hand-rolled runner. `golang-migrate`'s `database/sqlite` driver package imports `modernc.org/sqlite` directly (confirmed by inspecting its import graph) rather than the CGO-based `mattn/go-sqlite3` driver some might expect — so using it does not compromise the no-CGO/static-binary requirement. Given that, there was no reason to hand-roll a runner: versioned up/down SQL files, embedded via `embed.FS` and the `iofs` source driver, is exactly the "minimal reasonable implementation" the spec asks for when a decision doesn't materially matter.

### 1.3 Token-at-rest encryption: AES-256-GCM
`TOKEN_ENCRYPTION_KEY` (32 bytes, base64 or hex) is used directly as an AES-256 key; each encrypted value is `nonce (96 bits, random per call) || ciphertext || GCM tag`, base64-encoded for SQLite TEXT storage. AES-GCM was chosen over a non-authenticated mode because it detects tampering/corruption on decrypt (returns an error) rather than silently producing garbage that would then be sent to Emby as a bearer token.

### 1.3.1 CSRF coverage and where "join" fits

The spec lists join among the state-changing actions needing CSRF protection. In this implementation, joining a party is not a separate HTTP endpoint — it happens as part of the WebSocket handshake itself (`GET /ws/parties/{id}`), which re-validates the user's media authorization and is protected by Origin validation instead (browsers don't attach custom headers to a WebSocket upgrade the way a synchronizer token would need, so Origin checking — already required by the spec for this endpoint — is the applicable defense here, not CSRF). Every other state-changing action the spec calls out (create party, transfer host, end party, logout, leave) is a regular JSON POST and does carry the CSRF header requirement.

### 1.4 CSRF: synchronizer token, not double-submit-cookie
The spec explicitly says not to introduce a `SESSION_SECRET` unless something actually needs signing, and that CSRF may need its own key only if a double-submit-cookie (HMAC-based) pattern is used. Since sessions are already server-side and opaque, Watch Party generates a second random 256-bit token at session-creation time, stores it alongside the session row, and requires it to be echoed back in an `X-CSRF-Token` header on every state-changing request (compared with a constant-time comparison). This needs no signing key at all — one fewer secret to manage — and is exactly as strong as a synchronizer token is expected to be, since the token is bound server-side to the session rather than derived from anything guessable.

### 1.5 Session timestamps: RFC3339Nano wall-clock, not monotonic
Per the spec's own guidance: Go's monotonic clock reading is stripped once a `time.Time` is serialized, so a `server_timestamp` sent over the WebSocket as RFC3339Nano text is a wall-clock value, not a monotonic guarantee. This project accepts that as a deliberate, documented simplification — sub-second drift thresholds and typical NTP slewing behavior mean the rare wall-clock adjustment does not meaningfully break the sync experience — rather than building a custom monotonic-timestamp wire format just to sidestep a JSON-serialization detail. The same RFC3339Nano text format is reused for all other stored timestamps (session expiry, party creation, etc.) purely for consistency and human-readability in the database; those have no monotonicity requirement to begin with.

### 1.6 Sync math: implemented once in Go, mirrored (not shared) in the browser client
The frontend is vanilla JS with no bundler, so there is no mechanism to share code with the Go backend across that boundary. Clock-offset estimation, expected-position/drift calculation, and the rate-nudge-vs-hard-seek decision are implemented as pure, thoroughly unit-tested functions in `internal/syncalg`, which serves as the canonical reference and is also what the server itself uses for its debug-level sync diagnostics logging. The same algorithm is re-implemented in `web/static/js/sync.js` for the browser, since only the browser can actually drive a `<video>` element's `playbackRate`. This is intentional, acknowledged duplication of a small, stable algorithm — not an attempt to avoid it — because the alternative (a build step to share code, or a WASM bridge) would be a disproportionate amount of tooling for ~150 lines of arithmetic, and would cut against the spec's "no frontend build pipeline" constraint.

---

## 2. Session implementation

Opaque, cryptographically random 256-bit session ID (hex-encoded), stored `HttpOnly` and `SameSite=Lax` always, `Secure` whenever the issuing request looks like it arrived over HTTPS. The session ID is looked up server-side in SQLite on every request; no JWT, no signing secret. The Emby `AccessToken` itself lives on the `users` row (encrypted, see §1.3), not on the session — a user's token is refreshed in place on each login, and multiple concurrent sessions for the same user share it, which matches how Emby tokens actually work (they're not scoped to a single Watch Party session).

Two independent expiry mechanisms, both configurable: an idle timeout (`SESSION_IDLE_TIMEOUT`, sliding — refreshed on each authenticated request) and an absolute max age (`SESSION_MAX_AGE`, fixed at session creation). Either one being exceeded invalidates the session. If Emby itself rejects the stored token (401 on any Emby call made on the user's behalf), Watch Party deletes all of that user's sessions server-side and forces re-authentication, rather than retrying a dead token indefinitely.

### 2.1 Secure-cookie determination: per-request, not a `DEV_MODE` flag

**Original design, and why it changed.** The first version of this app decided whether to set the `Secure` cookie attribute from a single, deployment-wide `DEV_MODE` boolean: `Secure` unless `DEV_MODE=true`, which was meant only to unblock local `http://` testing. That design has a real gap it didn't account for: a legitimate self-hosted deployment can be reachable at more than one origin simultaneously with *different* schemes — e.g. an external `https://watchparty.example.com` through a reverse proxy, and an internal-only `http://watchparty.home` on the operator's own LAN that has no TLS certificate and doesn't need one. Under the old design there was no way to serve both correctly: `DEV_MODE=false` makes every cookie `Secure`, which a browser will silently refuse to store when the page was loaded over the HTTP origin (login appears to succeed but the session never persists); `DEV_MODE=true` makes every cookie non-`Secure`, weakening the HTTPS origin's cookie for no reason, directly contradicting the spec's own instruction not to relax the production requirement to accommodate this. A static, deployment-wide flag simply cannot get both origins right at once, because "is this cookie allowed to be `Secure`" is a property of the individual request, not of the deployment.

**Fix:** `Secure` is now computed per-request (`internal/session.isSecureRequest`): true if `r.TLS != nil` (this process is terminating TLS itself — not the normal case here) or the request carries `X-Forwarded-Proto: https` (the standard signal a reverse proxy sets, and the normal case for this project's documented Traefik-fronted deployment). This makes `DEV_MODE` unnecessary for its original purpose too: a bare `go run ./cmd/server` hit directly over `http://localhost:8080` has no `X-Forwarded-Proto` header at all, so it's indistinguishable from — and handled identically to — the internal-LAN-origin case, with zero configuration.

**Trust assumption, stated explicitly.** `X-Forwarded-Proto` is attacker-controllable input if the process is ever directly reachable by an untrusted client rather than strictly through the reverse proxy it's designed to sit behind. This is judged acceptable, not a new risk introduced by this change: the project's whole deployment model already assumes Watch Party is not directly internet-reachable (`docker-compose.yml` puts it on an internal Traefik network with no published port; `watchparty.container`'s example binds `PublishPort` to `127.0.0.1` only), and a forged header here can only make the server treat a request as *more* secure than it is — which a real browser would simply refuse to honor by declining to store the resulting `Secure` cookie — not less, so it cannot be used to downgrade a genuinely-HTTPS session's cookie.

`APP_ORIGINS` no longer rejects non-`https://` entries the way it used to (that check existed purely to prevent a `DEV_MODE`-style global weakening; it no longer applies to anything). A non-HTTPS entry now just produces an informational startup log line, since it's an intentional, supported configuration rather than a mistake to guard against.

---

## 3. WebSocket protocol design

One WebSocket connection per client; one server-side broadcast hub per active party, implemented as an actor: each party is a single goroutine reading from a buffered command channel, so all state mutation for a given party is inherently serialized without needing a mutex around the playback state itself (a `map[partyID]*Party` registry, guarded by a small mutex, exists only for looking up/creating/removing party actors — not for the state inside them). This was chosen over a plain mutex-guarded struct because the correctness property that matters most here — commands for one party are applied in a strict, well-defined order, with sequence numbers assigned exactly once — falls out for free from a single-threaded actor, whereas a mutex-guarded struct would need equal care taken by hand at every call site to get the same guarantee.

Every message carries a `protocol_version` field (see `internal/wsproto`). Messages are grouped into three categories, kept conceptually and structurally distinct:
- **Control events** (host-only, authoritative): `play`, `pause`, `seek`, `host_transfer`. These are requests — the server validates host authorization, timestamps and sequences the resulting state change, applies it to the in-memory party state, and broadcasts the result. The broadcast, not the request, is what every client (including the host's own) treats as authoritative.
- **Lifecycle events**: `join`, `leave`.
- **Transport/control frames**: `ping`/`pong`/`heartbeat`, `clock_sync`, `snapshot`.

Every authoritative state update (every control-event broadcast, and every `snapshot`) carries the same four fields: `position_ticks`, `is_playing`, `server_timestamp`, `sequence_number`. `sequence_number` is a per-party counter, incremented exactly once per applied state change by the single-threaded party actor; clients reject any update whose sequence number is not strictly greater than the last one they applied (`internal/syncalg.IsStale`), which is what prevents a delayed `seek` from clobbering a newer `play`, or a stale snapshot from undoing a fresher one.

---

## 4. Synchronization algorithm

See `internal/syncalg` (canonical, unit-tested implementation) and §1.6 above for why the same math is mirrored in the browser client.

- **Clock offset / RTT**: standard two-timestamp estimator using the client's `t0`/`t3` and the server's echoed `t1`/`t2` — `RTT = (t3-t0) - (t2-t1)`, `offset = ((t1-t0) + (t2-t3)) / 2`. More accurate than assuming one-way latency is simply `RTT/2`.
- **Expected position**: `authoritative_position + (server_now - authoritative_timestamp)` while playing, where `server_now = local_now + clock_offset`; simply `authoritative_position` while paused (time doesn't advance).
- **Drift correction**: drift below `SYNC_SOFT_DRIFT_MS` → no correction. Between soft and `SYNC_HARD_DRIFT_MS` → nudge `playbackRate` (magnitude scales with how far into that range the drift falls, capped at `SYNC_MAX_RATE_ADJUSTMENT`), which avoids a visible jump; once drift falls back under the soft threshold, rate returns exactly to `1.0` rather than lingering off-speed. Beyond the hard threshold → hard seek, since a rate nudge would take too long to catch up. All three thresholds plus the snapshot interval are environment-configurable, not hardcoded.

---

## 5. Browser playback strategy

The video element's `src` (Direct Play/Stream) or HLS player source (transcoded) is constructed per §0.2, using the viewing user's own `AccessToken` as an `api_key` query parameter — never a shared service token. Client-generated player events that were themselves caused by a server-issued sync command (e.g. the `<video>` element firing its own `play` when the client called `.play()` in response to a broadcast) are suppressed from being re-sent to the server as new commands, using a short-lived "expecting this event" flag set immediately before the programmatic call and cleared once the corresponding native event fires — this is what prevents a feedback loop between server-driven state changes and the browser's native media element events.

### 5.1 Two real bugs found via a live deployment, and the fix

A real user's first playback attempt failed instantly with the browser's generic `MEDIA_ERR_SRC_NOT_SUPPORTED` ("was not suitable"). Two separate, compounding bugs were responsible:

**Bug 1 — JSON casing mismatch.** `emby.PlaybackURLResult` had no JSON struct tags, so it serialized with Go's default field names: `{"URL": ..., "IsTranscoded": ...}`. `player.js`, written against the intended snake_case convention the rest of this API uses, read `playback.url` (lowercase) — `undefined`, for every single login, regardless of the item's format. `video.src = undefined` becomes the literal string `"undefined"`, which the browser correctly rejects. This alone explained the reported symptom. Fixed by adding explicit `json:"url"`, `json:"is_transcoded"`, etc. tags, and by pinning the wire shape with a dedicated test (`TestPlaybackURLResult_WireCasingMatchesFrontend`) — a Go-side unit test alone couldn't have caught this, since the Go code was internally consistent (struct field access on the Go side always worked); only a test that round-trips through actual JSON and checks the keys a JS reader would use catches this class of bug.

**Bug 2 — no device capability negotiation.** `GetPlaybackURL` called `PlaybackInfo` with a plain `GET` and no `DeviceProfile`. Without one, Emby has no idea it's being asked on behalf of a browser and returns `SupportsDirectStream`/`SupportsDirectPlay` based on raw server capability — "can I serve these bytes as-is" — which is almost always yes, including for containers like `.mkv` that no browser's `<video>` element can open at all, regardless of the codec inside. The fix switches to `POST /Items/{id}/PlaybackInfo` with a `DeviceProfile` describing what a generic modern browser can actually decode natively (conservatively: H.264/AAC in MP4, VP8/VP9/AV1/Opus in WebM), routing everything else through an HLS transcoding profile — the same mechanism every real Emby/Jellyfin web client uses. This is the correct, Emby-native way to make this decision, rather than trying to reimplement container/codec sniffing client-side.

That second fix has a consequence: transcoded output is always HLS (`.m3u8`), which only Safari plays natively in a `<video>` element. Chrome, Firefox, and Edge need a JS demuxer feeding Media Source Extensions. **hls.js** (Apache-2.0) is vendored as a pre-built, non-module `<script>` (`web/static/js/vendor/hls.min.js` — see `vendor/README.md`) rather than pulled in via a package manager at build time, consistent with this project having no frontend build pipeline (§1.6): `player.js` feature-detects native HLS support (`video.canPlayType(...)`) and only invokes `Hls` when the browser actually needs it, so Safari's path is unaffected.

**Also fixed while in the area:** when Emby's `PlaybackInfo` response includes its own `TranscodingUrl` for the transcode case (it does, once a `DeviceProfile` is supplied), that URL is now used verbatim (host-prefixed, `api_key` appended) instead of hand-constructing a `master.m3u8` URL from scratch — more robust, since Emby's own URL already carries whatever parameters its transcoding pipeline actually needs for that specific negotiation.

---

## 6. Container privilege model: PUID/PGID without a shell

**The decision to make.** Self-hosted users commonly expect a `PUID`/`PGID` pair (the linuxserver.io convention) so the container writes files as a UID/GID that matches whatever owns a bind-mounted host directory, rather than some arbitrary container-internal ID the host user can't read or clean up. The image was originally built on `gcr.io/distroless/static-debian12:nonroot` (see the original Dockerfile), which hardcodes UID/GID 65532 with no shell to change it at runtime — the classic way other images support `PUID`/`PGID` (start as root, `chown` the volume, then `su-exec`/`gosu` down to the target user before exec'ing the real process) is unavailable, since that whole pattern depends on a shell and a small setuid helper binary, and distroless deliberately has neither.

**Alternatives considered:**
- **Switch the final stage to a minimal shell-having base (e.g. Alpine) with a `su-exec`-based entrypoint script.** This is the standard, well-trodden approach, but it gives up the "no shell, no package manager, minimal attack surface" property that was a deliberate earlier choice (§ Dockerfile comments) — trading a meaningful chunk of the image's reduced attack surface for a feature that a few dozen lines of Go can provide instead.
- **Drop privileges in-process in the Go binary itself, no shell required.** Chosen. The server, while still root, `chown`s its data directory to `PUID:PGID` and then permanently drops privileges to that UID/GID before opening the database or listening on anything — implemented in `internal/privdrop`.

**The part that's easy to get wrong.** A naive privilege drop in Go — a single `syscall.Setuid`/`syscall.Setgid` call — only changes credentials for the *calling OS thread*. The Go runtime spreads a process across multiple OS threads from the moment it starts, so a naive drop can leave other threads (and thus, eventually, other goroutines) still running as root, which is a real, documented Go pitfall, not a hypothetical one. `internal/privdrop` uses `syscall.AllThreadsSyscall` (added in Go 1.16 specifically for this class of problem) to apply `setgroups(0)` → `setgid` → `setuid` atomically across every OS thread. This requires `CGO_ENABLED=0`: `AllThreadsSyscall`'s own documentation states it "always returns ENOTSUP in binaries that use cgo," since it can't see threads created by cgo-linked code — which this project already builds with regardless, since `modernc.org/sqlite` is a pure-Go driver (§1.2). `internal/privdrop`'s tests exercise the real syscalls (in a re-exec'd subprocess, so the drop's irreversibility can't affect the test runner itself) rather than mocking the mechanism, specifically because this is the kind of code where a plausible-looking mock would hide exactly the bug that matters.

**Default.** `PUID`/`PGID` default to `65532` each when unset — the same UID the image used to hardcode via the `:nonroot` tag — so upgrading without setting either variable preserves existing file ownership and keeps the "secure by default, no configuration required" property. If the resolved UID or GID is `0`, the server logs a warning and continues (an operator may have a legitimate niche reason to want this) rather than refusing to start, since silently overriding an explicit operator choice would be its own kind of surprising behavior.
