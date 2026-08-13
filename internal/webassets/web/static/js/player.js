// Party page: fetches party state + playback URL, points <video> at Emby
// directly (the browser streams from Emby using the viewer's own token —
// this server never touches media bytes), and keeps playback in sync using
// the server-authoritative WebSocket protocol. The server's state is the
// single source of truth; this file is a thin renderer of it.
import { api } from "./api.js";
import { PartyConnection } from "./ws-client.js";
import {
  TICKS_PER_SECOND,
  estimateClockOffset,
  expectedPositionTicks,
  classifyDrift,
  recommendedPlaybackRate,
  isStale,
  DriftAction,
} from "./sync.js";

const partyId = window.WATCH_PARTY_ID;
const video = document.getElementById("video");
const statusEl = document.getElementById("status");
const membersEl = document.getElementById("members");
const hostControls = document.getElementById("host-controls");
const playBtn = document.getElementById("play-btn");
const pauseBtn = document.getElementById("pause-btn");
const fullscreenBtn = document.getElementById("fullscreen-btn");
const endBtn = document.getElementById("end-btn");
const leaveBtn = document.getElementById("leave-btn");
const errorEl = document.getElementById("error");

// Sync tuning: defaults match the server's, but the server is the actual
// authority — these only govern this client's own correction behavior and
// are safe to keep as sane client-side constants rather than plumbing them
// over the wire, since a mismatched client just corrects a bit more or less
// aggressively, never incorrectly.
const thresholds = { softDriftMs: 300, hardDriftMs: 1500, maxRateAdjustment: 0.05 };

let me = null;
let hostUserId = null;
let currentSeq = -1;
let currentState = null; // { positionTicks, isPlaying, serverTimestampMs }
let clockOffsetMs = 0;
let conn = null;
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

function setStatus(text) {
  statusEl.textContent = text;
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
  if (isHost()) sendControl("pause", video.currentTime);
});
video.addEventListener("seeked", () => {
  if (suppress.seeking) { suppress.seeking = false; return; }
  if (isHost()) sendControl("seek", video.currentTime);
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
video.addEventListener("loadedmetadata", recalibrateAndReseek, { once: true });
video.addEventListener("canplay", recalibrateAndReseek, { once: true });

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
  }
});

leaveBtn.addEventListener("click", async () => {
  try {
    await api(`/api/parties/${encodeURIComponent(partyId)}/leave`, { method: "POST" });
  } catch {
    // best-effort; navigate away regardless
  }
  window.location.href = "/";
});

function renderMembers(members) {
  membersEl.innerHTML = "";
  for (const m of members) {
    const li = document.createElement("li");
    li.className = `member ${m.connection_status}`;
    li.textContent = `${m.display_name}${m.is_host ? " (host)" : ""} — ${m.connection_status}`;
    if (isHost() && !m.is_host && m.connection_status === "connected") {
      const btn = document.createElement("button");
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
      li.appendChild(btn);
    }
    membersEl.appendChild(li);
  }
  hostControls.hidden = !isHost();
  // Ending the party is host-only, enforced server-side (a non-host's
  // request 403s — see ARCHITECTURE.md §3/handleEndParty), but there's no
  // reason to show a participant a button that can only ever error out for
  // them. Leaving, unlike ending, is available to everyone and stays
  // unconditionally visible.
  endBtn.hidden = !isHost();
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
    renderMembers(p.members || []);
    applyAuthoritativeState(state, { hardSeek: true });
  } else if (env.type === "seek") {
    applyAuthoritativeState(state, { hardSeek: true });
  } else {
    applyAuthoritativeState(state, { hardSeek: false });
  }
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
    setStatus("Could not load party: " + err.message);
    return;
  }
  hostUserId = partyInfo.host_user_id;
  itemDurationTicks = partyInfo.duration_ticks || 0;
  renderMembers(partyInfo.members || []);

  let playback;
  try {
    playback = await api(`/api/parties/${encodeURIComponent(partyId)}/playback-url`);
  } catch (err) {
    setStatus("Could not get a playback URL from Emby: " + err.message);
    return;
  }
  // See the requestedStartPositionTicks/playbackOffsetTicks declarations
  // above: 0 for direct play/stream, and for transcoded playback the item
  // offset Emby was *asked* to start this transcode session at (the
  // party's position at the moment this request was made). playbackOffsetTicks
  // starts equal to this as a best guess and gets corrected once the real
  // stream loads — see calibratePlaybackOffset.
  requestedStartPositionTicks = playback.start_position_ticks || 0;
  playbackOffsetTicks = requestedStartPositionTicks;
  attachSource(playback.url, playback.is_transcoded);

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
        case "clock_sync": {
          const p = env.payload;
          const t3 = Date.now();
          const { rttMs, offsetMs } = estimateClockOffset(p.t0, p.t1, p.t2, t3);
          clockOffsetMs = offsetMs;
          lastRttMs = rttMs;
          lastOffsetMs = offsetMs;
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
    (state) => setStatus(state === "open" ? "Connected" : state === "connecting" ? "Connecting…" : "Disconnected — reconnecting…")
  );

  startClockSync();
  startDriftLoop();
}

main();
