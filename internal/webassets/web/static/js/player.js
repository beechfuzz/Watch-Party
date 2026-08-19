// Party page: fetches party state + playback URL, points <video> at Emby
// directly (the browser streams from Emby using the viewer's own token —
// this server never touches media bytes), and keeps playback in sync using
// the server-authoritative WebSocket protocol. The server's state is the
// single source of truth; this file is a thin renderer of it.
import { api } from "./api.js";
import { PartyConnection } from "./ws-client.js";
import { initPlaylist, loadPlaylist } from "./playlist.js";
import { wireSettingsForm } from "./settingsForm.js";
import {
  TICKS_PER_SECOND,
  estimateClockOffset,
  expectedPositionTicks,
  classifyDrift,
  recommendedPlaybackRate,
  isStale,
  DriftAction,
} from "./sync.js";
import { isRealPause } from "./playback-events.js";

const partyId = window.WATCH_PARTY_ID;
const video = document.getElementById("video");
const playerIdleEl = document.getElementById("player-idle");
const partyTitleEl = document.getElementById("party-title");
const partySubtitleEl = document.getElementById("party-subtitle");
const syncBadge = document.getElementById("sync-badge");
const syncText = document.getElementById("sync-text");
const membersEl = document.getElementById("members");
const participantsHeading = document.getElementById("participants-heading");
const hostControls = document.getElementById("host-controls");
const playBtn = document.getElementById("play-btn");
const pauseBtn = document.getElementById("pause-btn");
const fullscreenBtn = document.getElementById("fullscreen-btn");
const inviteBtn = document.getElementById("invite-btn");
const endBtn = document.getElementById("end-btn");
const leaveBtn = document.getElementById("leave-btn");
const errorEl = document.getElementById("error");

const partySettingsBtn = document.getElementById("party-settings-btn");
const partySettingsDialog = document.getElementById("party-settings-dialog");
const partySettingsForm = document.getElementById("party-settings-form");
const partySettingsError = document.getElementById("party-settings-error");
const cancelPartySettingsBtn = document.getElementById("cancel-party-settings-btn");
const settingsPartyNameInput = document.getElementById("settings-party-name");
const partySettingsFields = wireSettingsForm({
  autoAdvance: "settings-auto-advance", showNextDialog: "settings-show-next-dialog",
  autoplayEnabled: "settings-autoplay-enabled", autoplayDelay: "settings-autoplay-delay",
});

const nextItemBanner = document.getElementById("next-item-banner");
const nextItemBannerText = document.getElementById("next-item-banner-text");

// Sync tuning: defaults match the server's, but the server is the actual
// authority — these only govern this client's own correction behavior and
// are safe to keep as sane client-side constants rather than plumbing them
// over the wire, since a mismatched client just corrects a bit more or less
// aggressively, never incorrectly.
const thresholds = { softDriftMs: 300, hardDriftMs: 1500, maxRateAdjustment: 0.05 };

let me = null;
let hostUserId = null;
let currentItemId = ""; // "" means idle: nothing currently loaded
let currentSeq = -1;
let currentState = null; // { positionTicks, isPlaying, serverTimestampMs }
let clockOffsetMs = 0;
let conn = null;
let currentPartySettings = { auto_advance: true, show_next_dialog: true, autoplay_enabled: true, autoplay_delay_seconds: 5 };
let pendingDeadlineMs = null; // null when no countdown is running
let countdownRenderTimer = null;
let suppress = { play: false, pause: false, seeking: false };

// A transcoded stream that starts mid-item (the common case: anyone but the
// very first person to load the page) doesn't hand back a video.currentTime
// that's a simple, predictable function of what was requested -- besides
// whether Emby resets its output timeline to 0 or keeps absolute
// timestamps, Emby can only start an HLS segment on a keyframe, so the
// stream's real start is silently snapped to the nearest keyframe at or
// before whatever position was actually requested (see ARCHITECTURE.md
// §5.9). Guessing at either of these produced a real, persistent offset
// error for actual users (§5.6, §5.9), so playbackOffsetTicks is
// *calibrated* from what the browser actually reports once the stream has
// loaded (see calibratePlaybackOffset) -- requestedStartPositionTicks (from
// the backend's start_position_ticks, always 0 for direct play/stream) is
// only the best guess used until that first calibration lands.
// itemPositionTicks/videoSeconds are the two-way conversion between "where
// this video element thinks it is" and "where the party actually is" that
// every position sent to or read from the server must go through.
let requestedStartPositionTicks = 0;
let itemDurationTicks = 0;
let playbackOffsetTicks = 0;

function itemPositionTicks(videoCurrentTimeSeconds) {
  return playbackOffsetTicks + Math.round(videoCurrentTimeSeconds * TICKS_PER_SECOND);
}

function videoSeconds(itemTicks) {
  return ticksToSeconds(Math.max(0, itemTicks - playbackOffsetTicks));
}

// The robust way to find out exactly where in the item this specific
// stream actually starts: compare how much playable content it reports
// (video.duration) against the item's real, known total duration
// (itemDurationTicks, from the party info fetched in main()). The gap
// between them is exactly how much was skipped at the start -- however
// that skip happened. An earlier version of this calibrated from
// video.seekable.start(0) instead, comparing it against
// requestedStartPositionTicks; that correctly handled the reset-vs-absolute
// question but had no way to detect keyframe snapping, since a snapped
// start looks the same in seekable.start(0) either way -- it only shows up
// as *less total content than expected*, which is exactly what comparing
// against the item's real duration catches.
function calibratePlaybackOffset() {
  if (!itemDurationTicks || !Number.isFinite(video.duration)) return;
  const videoDurationTicks = Math.round(video.duration * TICKS_PER_SECOND);
  playbackOffsetTicks = Math.max(0, itemDurationTicks - videoDurationTicks);
}

function isHost() {
  return me && hostUserId === me.user.id;
}

function setConnectionState(state) {
  syncBadge.classList.toggle("is-disconnected", state !== "open");
  if (state === "open") {
    syncText.textContent = lastRttMs !== null ? `In sync · ${Math.round(lastRttMs)}ms` : "In sync";
  } else if (state === "connecting") {
    syncText.textContent = "Connecting…";
  } else {
    syncText.textContent = "Reconnecting…";
  }
}

function showError(text) {
  errorEl.textContent = text;
  errorEl.hidden = false;
  setTimeout(() => { errorEl.hidden = true; }, 5000);
}

function programmaticPlay() {
  suppress.play = true;
  video.play().catch(() => {
    // Autoplay blocked (no user gesture yet) -- nothing to do here beyond
    // resetting the suppress flag. The always-visible native play button
    // (see the party.html <video controls> attribute) is what lets the
    // user actually start playback themselves; see ARCHITECTURE.md §5.10
    // for why this replaced a custom overlay/button doing the same job.
    suppress.play = false;
  });
}

function programmaticPause() {
  suppress.pause = true;
  video.pause();
}

function programmaticSeek(seconds) {
  suppress.seeking = true;
  video.currentTime = Math.max(0, seconds);
}

function ticksToSeconds(ticks) {
  return ticks / TICKS_PER_SECOND;
}

// attachSource points the <video> element at the Emby playback URL. Direct
// play/stream URLs (a plain media file Emby is serving as-is) just become
// video.src — the browser handles those natively. Transcoded URLs are
// always HLS (.m3u8): only Safari plays that format natively in a <video>
// element, so everywhere else needs hls.js (vendored — see
// vendor/README.md and ARCHITECTURE.md §5) to demux and feed it in via
// Media Source Extensions.
function attachSource(url, isTranscoded) {
  armRecalibration();
  if (!isTranscoded) {
    video.src = url;
    return;
  }
  const nativelySupportsHls = video.canPlayType("application/vnd.apple.mpegurl") !== "";
  if (nativelySupportsHls) {
    video.src = url;
    return;
  }
  if (typeof Hls === "undefined" || !Hls.isSupported()) {
    showError("This browser can't play transcoded video (no native HLS support and hls.js is unavailable).");
    return;
  }
  const hls = new Hls();
  hls.on(Hls.Events.ERROR, (_event, data) => {
    if (data.fatal) {
      showError("Playback error: " + (data.details || data.type));
    }
  });
  hls.loadSource(url);
  hls.attachMedia(video);
}

function applyAuthoritativeState(state, { hardSeek } = {}) {
  currentState = state;
  if (hardSeek) {
    const expected = expectedPositionTicks(state, Date.now(), clockOffsetMs);
    programmaticSeek(videoSeconds(expected));
    video.playbackRate = 1.0;
  }
  if (state.isPlaying && video.paused) {
    programmaticPlay();
  } else if (!state.isPlaying && !video.paused) {
    programmaticPause();
  }
}

// --- native video event -> host command forwarding ---
// Native controls (play/pause/seek bar) are visible to every participant,
// not just the host (see the <video controls> attribute in party.html) --
// but only the host's interactions with them are actually forwarded here.
// A participant playing with their own native controls only ever affects
// their own local view; it's never sent to the server, and the drift loop
// silently corrects any local deviation back within a second or two, same
// as it would correct any other drift. See ARCHITECTURE.md §5.10 for why
// this replaced a host-only toggle plus a separate custom overlay button.
// Each listener also checks suppress first, skipping forwarding entirely
// when the event was caused by our own programmatic call above — this is
// what prevents a feedback loop between server-driven changes and the
// browser's native media element events.
video.addEventListener("play", () => {
  if (suppress.play) { suppress.play = false; return; }
  if (isHost()) sendControl("play", video.currentTime);
});
video.addEventListener("pause", () => {
  if (suppress.pause) { suppress.pause = false; return; }
  // Reaching the natural end of the media also fires "pause" (with `ended`
  // already true) -- that's the browser observing playback stopped, not the
  // host asking to pause. Forwarding it as a real command used to set the
  // server's IsPlaying to false before the server's own end-of-media
  // detection ever ran, permanently blocking auto-advance/the next-item
  // dialog for that item. See playback-events.js.
  if (isHost() && isRealPause(video)) sendControl("pause", video.currentTime);
});
video.addEventListener("seeked", () => {
  if (suppress.seeking) { suppress.seeking = false; return; }
  if (isHost()) sendControl("seek", video.currentTime);
});
// The server is the sole authority for detecting end-of-media (periodic
// snapshotTicker, see checkEndOfMedia) -- this "ended" hint never asserts
// anything itself, it just prompts the server to run that same check
// immediately instead of waiting up to SYNC_SNAPSHOT_INTERVAL. See
// HandleEndedHint's doc comment and SPEC.md's Synchronization Protocol
// section ("the host client's ended event is an observation, not an
// authoritative command").
video.addEventListener("ended", () => {
  if (isHost()) conn.send("ended_hint");
});

// Recalibrate playbackOffsetTicks once the browser actually knows this
// session's real seekable range, and immediately re-seek to the currently
// expected position using the corrected offset -- any earlier seek
// (attempted before this fires, using only the pre-calibration guess) may
// have landed at the wrong spot. Listening on both events since it's not
// guaranteed which one first has a populated video.seekable across
// browsers/hls.js; recalibrating and reseeking again on the second is a
// harmless no-op if the first already got it right. { once: true } on both
// is load-bearing, not just tidiness: unlike loadedmetadata, canplay is not
// a fires-once event -- it refires every time playback recovers from a
// stall, which happens routinely on real network video. Without once:true
// here, every stall recovery re-triggered a fresh programmatic seek, which
// is indistinguishable from constant involuntary seeking -- i.e. choppy
// playback and a scrub bar that fights the user's own input. See
// ARCHITECTURE.md §5.8.
function recalibrateAndReseek() {
  calibratePlaybackOffset();
  if (currentState) {
    const expected = expectedPositionTicks(currentState, Date.now(), clockOffsetMs);
    programmaticSeek(videoSeconds(expected));
  }
}
// Armed fresh by attachSource on every item load (initial and every
// subsequent playlist item change) rather than registered once here at
// module scope -- a party's current item now changes over its lifetime, and
// each new item's stream needs its own calibration pass.
function armRecalibration() {
  video.addEventListener("loadedmetadata", recalibrateAndReseek, { once: true });
  video.addEventListener("canplay", recalibrateAndReseek, { once: true });
}

fullscreenBtn.addEventListener("click", () => {
  if (video.requestFullscreen) {
    video.requestFullscreen().catch((err) => showError("Couldn't enter fullscreen: " + err.message));
  } else if (video.webkitEnterFullscreen) {
    // iOS Safari: only the <video> element itself can go fullscreen, via
    // this legacy prefixed method instead of the standard Fullscreen API.
    video.webkitEnterFullscreen();
  } else {
    showError("Fullscreen isn't supported in this browser.");
  }
});

inviteBtn.addEventListener("click", async () => {
  const link = `${window.location.origin}/party/${encodeURIComponent(partyId)}`;
  try {
    await navigator.clipboard.writeText(link);
    const original = inviteBtn.title;
    inviteBtn.title = "Copied!";
    setTimeout(() => { inviteBtn.title = original; }, 2000);
  } catch {
    showError("Couldn't copy the invite link — copy it from the address bar instead.");
  }
});

function sendControl(type, currentTimeSeconds) {
  conn.send(type, { position_ticks: itemPositionTicks(currentTimeSeconds) });
}

playBtn.addEventListener("click", () => sendControl("play", video.currentTime));
pauseBtn.addEventListener("click", () => sendControl("pause", video.currentTime));

endBtn.addEventListener("click", async () => {
  if (!confirm("End this party for everyone?")) return;
  try {
    await api(`/api/parties/${encodeURIComponent(partyId)}/end`, { method: "POST" });
  } catch (err) {
    showError(err.message);
    return;
  }
  window.location.href = "/";
});

leaveBtn.addEventListener("click", async () => {
  try {
    await api(`/api/parties/${encodeURIComponent(partyId)}/leave`, { method: "POST" });
  } catch {
    // best-effort; navigate away regardless
  }
  window.location.href = "/";
});

partySettingsBtn.addEventListener("click", () => {
  partySettingsError.hidden = true;
  settingsPartyNameInput.value = partyTitleEl.textContent;
  partySettingsFields.write(currentPartySettings);
  partySettingsDialog.showModal();
});
cancelPartySettingsBtn.addEventListener("click", () => partySettingsDialog.close());
partySettingsForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  partySettingsError.hidden = true;
  try {
    await api(`/api/parties/${encodeURIComponent(partyId)}/settings`, {
      method: "PUT",
      body: { name: settingsPartyNameInput.value, ...partySettingsFields.read() },
    });
    // The resulting snapshot broadcast (see Party.UpdateSettings) updates
    // partyTitleEl/currentPartySettings for every connected client,
    // including this one -- no need to apply the new values locally here.
    partySettingsDialog.close();
  } catch (err) {
    partySettingsError.textContent = err.message;
    partySettingsError.hidden = false;
  }
});

function initials(name) {
  return (name || "?").trim().slice(0, 1).toUpperCase();
}

function renderMembers(members) {
  membersEl.innerHTML = "";
  const connectedCount = members.filter((m) => m.connection_status === "connected").length;
  participantsHeading.textContent = `Watching now — ${connectedCount}`;

  for (const m of members) {
    const row = document.createElement("div");
    row.className = `participant-row${m.connection_status !== "connected" ? " is-offline" : ""}`;
    row.innerHTML = `
      <div class="pulse-avatar">
        <span class="pulse-ring"></span>
        <div class="avatar"></div>
      </div>
      <div>
        <div class="participant-name"></div>
        <div class="participant-status${m.connection_status === "connected" ? " online" : ""}"></div>
      </div>
    `;
    row.querySelector(".avatar").textContent = initials(m.display_name);
    const nameEl = row.querySelector(".participant-name");
    nameEl.append(m.display_name);
    if (m.is_host) {
      nameEl.insertAdjacentHTML("beforeend", ` <svg class="host-star" viewBox="0 0 24 24" fill="currentColor"><path d="M12 2l2.9 6.6 7.1.6-5.4 4.7 1.7 7-6.3-3.9L5.7 21l1.7-7-5.4-4.7 7.1-.6L12 2z"/></svg>`);
    }
    row.querySelector(".participant-status").textContent =
      m.connection_status === "connected" ? "In sync" : m.connection_status === "disconnected" ? "Reconnecting…" : "Left";

    if (isHost() && !m.is_host && m.connection_status === "connected") {
      const btn = document.createElement("button");
      btn.className = "make-host-btn";
      btn.textContent = "Make host";
      btn.addEventListener("click", async () => {
        try {
          await api(`/api/parties/${encodeURIComponent(partyId)}/host-transfer`, {
            method: "POST",
            body: { new_host_user_id: m.user_id },
          });
        } catch (err) {
          showError(err.message);
        }
      });
      row.appendChild(btn);
    }
    membersEl.appendChild(row);
  }
  updateIdleVisibility();
  // Ending the party is host-only, enforced server-side (a non-host's
  // request 403s — see ARCHITECTURE.md §3/handleEndParty), but there's no
  // reason to show a participant a button that can only ever error out for
  // them. Leaving, unlike ending, is available to everyone and stays
  // unconditionally visible. Party Settings edits are host-only too, same
  // reasoning.
  endBtn.hidden = !isHost();
  partySettingsBtn.hidden = !isHost();
}

// The party can be idle -- freshly created (playlist not started yet) or
// having just run out of queued items -- not just playing/paused. The
// player shows a placeholder instead of an empty/frozen <video> element,
// and host controls (which have nothing to act on) hide along with it.
function updateIdleVisibility() {
  const idle = !currentItemId;
  playerIdleEl.hidden = !idle;
  video.hidden = idle;
  hostControls.hidden = !isHost() || idle;
}

// The end-of-media countdown banner: server-driven (see
// ARCHITECTURE.md's Party Settings section) -- this only *renders* against
// an absolute deadline the server already computed, using the same
// clockOffsetMs the clock-sync handshake already maintains elsewhere in
// this file. It never decides when the transition happens; the next
// snapshot (the actual fired transition, or a host command that canceled
// the countdown) is what clears it, not a client-side timeout guess.
let pendingAutoplay = false;

function updateNextItemBanner(snap) {
  const pending = snap.pending_transition;
  if (!pending || !snap.show_next_dialog) {
    hideNextItemBanner();
    return;
  }
  pendingDeadlineMs = Date.parse(pending.deadline);
  pendingAutoplay = pending.autoplay;
  if (!countdownRenderTimer) {
    countdownRenderTimer = setInterval(renderCountdown, 250);
  }
  renderCountdown();
}

function hideNextItemBanner() {
  pendingDeadlineMs = null;
  if (countdownRenderTimer) {
    clearInterval(countdownRenderTimer);
    countdownRenderTimer = null;
  }
  nextItemBanner.hidden = true;
}

function renderCountdown() {
  if (pendingDeadlineMs === null) return;
  const remainingMs = pendingDeadlineMs - (Date.now() + clockOffsetMs);
  const remainingSeconds = Math.max(0, Math.ceil(remainingMs / 1000));
  const label = pendingAutoplay ? "Next item in" : "Playback pauses in";
  nextItemBannerText.innerHTML = `${label} <strong>${remainingSeconds}s</strong>…`;
  nextItemBanner.hidden = false;
}

function handleSnapshotOrControl(env) {
  const p = env.payload;
  if (isStale(currentSeq, p.sequence_number)) return;
  currentSeq = p.sequence_number;

  const state = {
    positionTicks: p.position_ticks,
    isPlaying: p.is_playing,
    serverTimestampMs: Date.parse(p.server_timestamp),
  };

  if (env.type === "snapshot") {
    hostUserId = p.host_user_id;
    itemDurationTicks = p.duration_ticks || 0;
    partyTitleEl.textContent = p.name;
    currentPartySettings = {
      auto_advance: p.auto_advance, show_next_dialog: p.show_next_dialog,
      autoplay_enabled: p.autoplay_enabled, autoplay_delay_seconds: p.autoplay_delay_seconds,
    };
    updateNextItemBanner(p);
    renderMembers(p.members || []);
    // The current item can change after join now (playlist advance, end of
    // playlist, or a host selecting something else) -- not just at load
    // time. A changed item_id means a new stream entirely: re-fetch (or
    // clear) the playback URL, which is also the point this participant's
    // access to the NEW item gets re-validated (see handlePlaybackURL and
    // ARCHITECTURE.md's Playlist section) -- and refresh the playlist
    // panel so its "Now playing" badge follows along.
    if (p.item_id !== currentItemId) {
      currentItemId = p.item_id;
      updateIdleVisibility();
      loadCurrentItem(currentItemId);
      loadPlaylist();
    }
    applyAuthoritativeState(state, { hardSeek: true });
  } else if (env.type === "seek") {
    applyAuthoritativeState(state, { hardSeek: true });
  } else {
    applyAuthoritativeState(state, { hardSeek: false });
  }
}

// Fetches (or clears, if itemId is empty -- idle) the playback URL for
// whatever's currently loaded and points <video> at it. Used both for the
// party's initial current item (from main()) and every subsequent item
// change (see handleSnapshotOrControl).
async function loadCurrentItem(itemId) {
  video.pause();
  video.removeAttribute("src");
  video.load();
  if (!itemId) return;

  let playback;
  try {
    playback = await api(`/api/parties/${encodeURIComponent(partyId)}/playback-url`);
  } catch (err) {
    showError("Could not get a playback URL from Emby: " + err.message);
    return;
  }
  requestedStartPositionTicks = playback.start_position_ticks || 0;
  playbackOffsetTicks = requestedStartPositionTicks;
  attachSource(playback.url, playback.is_transcoded);
}

// Last-computed diagnostics, piggybacked onto the next clock_sync ping so
// the server can log per-event sync diagnostics at LOG_LEVEL=debug (see
// httpapi.logSyncDiagnostics) without a dedicated wire message type.
let lastRttMs = null;
let lastOffsetMs = null;
let lastCorrectionAction = "none";

function startClockSync() {
  const doSync = () => {
    const payload = { t0: Date.now(), position_ticks: itemPositionTicks(video.currentTime) };
    if (lastRttMs !== null) payload.last_rtt_ms = Math.round(lastRttMs);
    if (lastOffsetMs !== null) payload.last_clock_offset_ms = Math.round(lastOffsetMs);
    payload.last_correction_action = lastCorrectionAction;
    conn.send("clock_sync", payload);
  };
  doSync();
  setInterval(doSync, 30000);
}

function startDriftLoop() {
  setInterval(() => {
    if (!currentState || video.paused || video.seeking) return;
    const expected = expectedPositionTicks(currentState, Date.now(), clockOffsetMs);
    const actual = itemPositionTicks(video.currentTime);
    const { driftTicks, action } = classifyDrift(actual, expected, thresholds);
    if (action === DriftAction.HARD_SEEK) {
      programmaticSeek(videoSeconds(expected));
      video.playbackRate = 1.0;
    } else if (action === DriftAction.NUDGE_RATE) {
      video.playbackRate = recommendedPlaybackRate(driftTicks, thresholds);
    } else {
      video.playbackRate = 1.0;
    }
    lastCorrectionAction = action;
  }, 1000);
}

async function main() {
  try {
    me = await api("/api/me");
  } catch {
    window.location.href = "/";
    return;
  }

  let partyInfo;
  try {
    partyInfo = await api(`/api/parties/${encodeURIComponent(partyId)}`);
  } catch (err) {
    partyTitleEl.textContent = "Could not load party";
    syncBadge.classList.add("is-disconnected");
    syncText.textContent = err.message;
    return;
  }
  hostUserId = partyInfo.host_user_id;
  itemDurationTicks = partyInfo.duration_ticks || 0;
  currentItemId = partyInfo.item_id || "";
  partyTitleEl.textContent = partyInfo.name;
  currentPartySettings = {
    auto_advance: partyInfo.auto_advance, show_next_dialog: partyInfo.show_next_dialog,
    autoplay_enabled: partyInfo.autoplay_enabled, autoplay_delay_seconds: partyInfo.autoplay_delay_seconds,
  };
  updateNextItemBanner(partyInfo); // in case the page loads mid-countdown (e.g. a refresh)
  const hostMember = (partyInfo.members || []).find((m) => m.is_host);
  partySubtitleEl.textContent = hostMember ? `Hosted by ${hostMember.display_name}` : "";
  renderMembers(partyInfo.members || []);

  initPlaylist(partyId, isHost);
  loadPlaylist();

  // See the requestedStartPositionTicks/playbackOffsetTicks declarations
  // above: 0 for direct play/stream, and for transcoded playback the item
  // offset Emby was *asked* to start this transcode session at (the
  // party's position at the moment this request was made). playbackOffsetTicks
  // starts equal to this as a best guess and gets corrected once the real
  // stream loads — see calibratePlaybackOffset. A freshly-created party (or
  // one whose playlist just ran out) has no current item at all yet --
  // loadCurrentItem no-ops in that case and the idle placeholder (already
  // shown by updateIdleVisibility, called from renderMembers above) stands
  // in for the player.
  await loadCurrentItem(currentItemId);

  conn = new PartyConnection(
    partyId,
    (env) => {
      switch (env.type) {
        case "snapshot":
        case "play":
        case "pause":
        case "seek":
          handleSnapshotOrControl(env);
          break;
        case "playlist_updated":
          loadPlaylist();
          break;
        case "clock_sync": {
          const p = env.payload;
          const t3 = Date.now();
          const { rttMs, offsetMs } = estimateClockOffset(p.t0, p.t1, p.t2, t3);
          clockOffsetMs = offsetMs;
          lastRttMs = rttMs;
          lastOffsetMs = offsetMs;
          setConnectionState("open");
          break;
        }
        case "pong":
          break;
        case "error":
          showError(env.payload && env.payload.message ? env.payload.message : "an error occurred");
          break;
        default:
          break;
      }
    },
    (state) => setConnectionState(state)
  );

  startClockSync();
  startDriftLoop();
}

main();
