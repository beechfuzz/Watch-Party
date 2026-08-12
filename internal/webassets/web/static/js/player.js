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

function applyAuthoritativeState(state, { hardSeek } = {}) {
  currentState = state;
  if (hardSeek) {
    const expected = expectedPositionTicks(state, Date.now(), clockOffsetMs);
    programmaticSeek(ticksToSeconds(expected));
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

startBtn.addEventListener("click", () => {
  startOverlay.hidden = true;
  video.play().catch((err) => showError("Could not start playback: " + err.message));
});

function sendControl(type, currentTimeSeconds) {
  conn.send(type, { position_ticks: Math.round(currentTimeSeconds * TICKS_PER_SECOND) });
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
    const payload = { t0: Date.now(), position_ticks: Math.round(video.currentTime * TICKS_PER_SECOND) };
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
    const actual = Math.round(video.currentTime * TICKS_PER_SECOND);
    const { driftTicks, action } = classifyDrift(actual, expected, thresholds);
    if (action === DriftAction.HARD_SEEK) {
      programmaticSeek(ticksToSeconds(expected));
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
  video.src = playback.url;

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
