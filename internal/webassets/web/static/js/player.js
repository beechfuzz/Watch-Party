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
const startOverlay = document.getElementById("start-overlay");
const startBtn = document.getElementById("start-playback-btn");
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
// very first person to load the page) has Emby reset the HLS timeline to 0
// rather than continuing from that offset -- see attachSource's caller in
// main() and ARCHITECTURE.md §5.4. playbackOffsetTicks is how far into the
// real item this video's local timeline (video.currentTime) starts;
// itemPositionTicks/videoSeconds below are the two-way conversion between
// "where this video element thinks it is" and "where the party actually
// is," which every position sent to or received from the server must go
// through. It's 0 (a no-op conversion) for direct play/stream, where
// video.currentTime is already an absolute item position.
let playbackOffsetTicks = 0;

function itemPositionTicks(videoCurrentTimeSeconds) {
  return playbackOffsetTicks + Math.round(videoCurrentTimeSeconds * TICKS_PER_SECOND);
}

function videoSeconds(itemTicks) {
  return ticksToSeconds(Math.max(0, itemTicks - playbackOffsetTicks));
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
    // Autoplay blocked: show the explicit user-gesture affordance.
    suppress.play = false;
    startOverlay.hidden = false;
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
// Only fires for the host (participants have controls disabled) and only
// when the event wasn't caused by our own programmatic call above — this
// is what prevents a feedback loop between server-driven changes and the
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

// The "start playback" overlay is only ever needed until the video is
// actually, verifiably playing — hiding it solely on the start button's
// own click (as before) left it stuck on screen over live video any time
// playback began some other way (e.g. an autoplay attempt that succeeded
// on a later authoritative state after an earlier one was blocked). This
// fires regardless of what caused playback to start, so it can't get out
// of sync with reality the way a per-code-path hide can.
video.addEventListener("playing", () => {
  startOverlay.hidden = true;
});

startBtn.addEventListener("click", () => {
  video.play().catch((err) => showError("Could not start playback: " + err.message));
});

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
  // Native controls (play/pause/seek bar/volume) are host-only — a
  // participant's native seeking wouldn't do anything but confuse them,
  // since only the host's play/pause/seek commands reach the server (see
  // the isHost() checks below); the drift loop would just correct a
  // participant's local scrub away within a second or two. Fullscreen is
  // available to everyone regardless, via the dedicated button.
  video.controls = isHost();
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
  renderMembers(partyInfo.members || []);

  let playback;
  try {
    playback = await api(`/api/parties/${encodeURIComponent(partyId)}/playback-url`);
  } catch (err) {
    setStatus("Could not get a playback URL from Emby: " + err.message);
    return;
  }
  // See the playbackOffsetTicks declaration above: 0 for direct play/
  // stream, and for transcoded playback the item offset Emby actually
  // started this specific transcode session at (the party's position at
  // the moment this request was made) — every position sent to or read
  // from the server has to be translated through this.
  playbackOffsetTicks = playback.start_position_ticks || 0;
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
