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

### 1.4 CSRF: synchronizer token, not double-submit-cookie
The spec explicitly says not to introduce a `SESSION_SECRET` unless something actually needs signing, and that CSRF may need its own key only if a double-submit-cookie (HMAC-based) pattern is used. Since sessions are already server-side and opaque, Watch Party generates a second random 256-bit token at session-creation time, stores it alongside the session row, and requires it to be echoed back in an `X-CSRF-Token` header on every state-changing request (compared with a constant-time comparison). This needs no signing key at all — one fewer secret to manage — and is exactly as strong as a synchronizer token is expected to be, since the token is bound server-side to the session rather than derived from anything guessable.

### 1.5 Session timestamps: RFC3339Nano wall-clock, not monotonic
Per the spec's own guidance: Go's monotonic clock reading is stripped once a `time.Time` is serialized, so a `server_timestamp` sent over the WebSocket as RFC3339Nano text is a wall-clock value, not a monotonic guarantee. This project accepts that as a deliberate, documented simplification — sub-second drift thresholds and typical NTP slewing behavior mean the rare wall-clock adjustment does not meaningfully break the sync experience — rather than building a custom monotonic-timestamp wire format just to sidestep a JSON-serialization detail. The same RFC3339Nano text format is reused for all other stored timestamps (session expiry, party creation, etc.) purely for consistency and human-readability in the database; those have no monotonicity requirement to begin with.

### 1.6 Sync math: implemented once in Go, mirrored (not shared) in the browser client
The frontend is vanilla JS with no bundler, so there is no mechanism to share code with the Go backend across that boundary. Clock-offset estimation, expected-position/drift calculation, and the rate-nudge-vs-hard-seek decision are implemented as pure, thoroughly unit-tested functions in `internal/syncalg`, which serves as the canonical reference and is also what the server itself uses for its debug-level sync diagnostics logging. The same algorithm is re-implemented in `web/static/js/sync.js` for the browser, since only the browser can actually drive a `<video>` element's `playbackRate`. This is intentional, acknowledged duplication of a small, stable algorithm — not an attempt to avoid it — because the alternative (a build step to share code, or a WASM bridge) would be a disproportionate amount of tooling for ~150 lines of arithmetic, and would cut against the spec's "no frontend build pipeline" constraint.

---

## 2. Session implementation

Opaque, cryptographically random 256-bit session ID (hex-encoded), stored `HttpOnly`, `SameSite=Lax`, and `Secure` in production (a `DEV_MODE` env var permits non-Secure cookies for local `http://` testing only — the production default is never weakened). The session ID is looked up server-side in SQLite on every request; no JWT, no signing secret. The Emby `AccessToken` itself lives on the `users` row (encrypted, see §1.3), not on the session — a user's token is refreshed in place on each login, and multiple concurrent sessions for the same user share it, which matches how Emby tokens actually work (they're not scoped to a single Watch Party session).

Two independent expiry mechanisms, both configurable: an idle timeout (`SESSION_IDLE_TIMEOUT`, sliding — refreshed on each authenticated request) and an absolute max age (`SESSION_MAX_AGE`, fixed at session creation). Either one being exceeded invalidates the session. If Emby itself rejects the stored token (401 on any Emby call made on the user's behalf), Watch Party deletes all of that user's sessions server-side and forces re-authentication, rather than retrying a dead token indefinitely.

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
