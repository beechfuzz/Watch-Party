// Pure sync math for the browser client: clock-offset/RTT estimation,
// expected-position drift calculation, and the rate-nudge-vs-hard-seek
// decision. This intentionally mirrors internal/syncalg (Go) rather than
// sharing code with it — see ARCHITECTURE.md §1.6 for why: the frontend
// has no bundler to share code with the Go backend through, and only the
// browser can actually drive a <video> element's playbackRate. Keep this
// file in sync with internal/syncalg/syncalg.go if either changes.
//
// No framework, no build step: plain ES module, loaded with a <script
// type="module"> tag. Runnable directly under Node's built-in test runner
// (see sync.test.mjs) without any extra tooling.

export const TICKS_PER_SECOND = 10_000_000; // Emby PositionTicks: 100ns units

/**
 * Two-timestamp clock-offset/RTT estimator, per the spec's handshake:
 * client sends t0, server echoes back t1 (its receive time) and t2 (its
 * send time), client records t3 (its receive time). All four are
 * milliseconds since Unix epoch on each side's own clock.
 *
 *   RTT    = (t3 - t0) - (t2 - t1)
 *   offset = ((t1 - t0) + (t2 - t3)) / 2   -- add to local time to estimate server time
 */
export function estimateClockOffset(t0, t1, t2, t3) {
  const rtt = (t3 - t0) - (t2 - t1);
  const offset = ((t1 - t0) + (t2 - t3)) / 2;
  return { rttMs: rtt, offsetMs: offset };
}

/**
 * Expected playback position right now (in PositionTicks), given the last
 * authoritative state and the estimated clock offset. Paused states don't
 * advance.
 *
 * @param {{positionTicks:number, isPlaying:boolean, serverTimestampMs:number}} state
 * @param {number} nowMs local clock reading (Date.now())
 * @param {number} offsetMs estimated clock offset (local + offset ~= server time)
 */
export function expectedPositionTicks(state, nowMs, offsetMs) {
  if (!state.isPlaying) {
    return state.positionTicks;
  }
  const serverNowMs = nowMs + offsetMs;
  let elapsedMs = serverNowMs - state.serverTimestampMs;
  if (elapsedMs < 0) elapsedMs = 0;
  return state.positionTicks + Math.round(elapsedMs * TICKS_PER_SECOND / 1000);
}

export const DriftAction = Object.freeze({
  NONE: "none",
  NUDGE_RATE: "nudge_rate",
  HARD_SEEK: "hard_seek",
});

function absTicksToMs(ticks) {
  return Math.abs(ticks) * 1000 / TICKS_PER_SECOND;
}

/**
 * @param {number} actualPositionTicks current <video>.currentTime, in ticks
 * @param {number} expectedTicks from expectedPositionTicks()
 * @param {{softDriftMs:number, hardDriftMs:number, maxRateAdjustment:number}} thresholds
 * @returns {{driftTicks:number, action:string}}
 */
export function classifyDrift(actualPositionTicks, expectedTicks, thresholds) {
  const driftTicks = actualPositionTicks - expectedTicks;
  const absMs = absTicksToMs(driftTicks);
  let action = DriftAction.NONE;
  if (absMs > thresholds.hardDriftMs) {
    action = DriftAction.HARD_SEEK;
  } else if (absMs > thresholds.softDriftMs) {
    action = DriftAction.NUDGE_RATE;
  }
  return { driftTicks, action };
}

/**
 * Recommended playbackRate for a given signed drift (positive = ahead of
 * schedule, needs to slow down; negative = behind, needs to speed up),
 * clamped to [1-maxAdjust, 1+maxAdjust] and scaled by how far into the
 * soft-to-hard range the drift falls. Exactly 1.0 when driftTicks is 0, so
 * a caller that keeps calling this as drift shrinks back toward zero will
 * naturally return exactly to normal speed rather than lingering off-speed.
 */
export function recommendedPlaybackRate(driftTicks, thresholds) {
  if (thresholds.hardDriftMs <= 0) return 1.0;
  const absMs = absTicksToMs(driftTicks);
  const fraction = Math.min(absMs / thresholds.hardDriftMs, 1);
  const adjust = thresholds.maxRateAdjustment * fraction;
  return driftTicks > 0 ? 1.0 - adjust : 1.0 + adjust;
}

/**
 * Whether an incoming authoritative state update should be rejected as
 * stale (its sequence number is not strictly newer than what's already
 * applied) — prevents a delayed seek from clobbering a newer play, or a
 * stale snapshot from undoing a fresher one.
 */
export function isStale(currentSequenceNumber, incomingSequenceNumber) {
  return incomingSequenceNumber <= currentSequenceNumber;
}
