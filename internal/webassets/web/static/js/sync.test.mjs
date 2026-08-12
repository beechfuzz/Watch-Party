// Run with: node --test internal/webassets/web/static/js/sync.test.mjs
// No package.json, no bundler, no test framework dependency — Node's
// built-in test runner is sufficient for this small, stable module. See
// ARCHITECTURE.md §1.6.
import test from "node:test";
import assert from "node:assert/strict";
import {
  TICKS_PER_SECOND,
  estimateClockOffset,
  expectedPositionTicks,
  classifyDrift,
  recommendedPlaybackRate,
  isStale,
  DriftAction,
} from "./sync.js";

test("estimateClockOffset: no latency, no offset", () => {
  const { rttMs, offsetMs } = estimateClockOffset(0, 10, 10, 20);
  assert.equal(rttMs, 20);
  assert.equal(offsetMs, 0);
});

test("estimateClockOffset: server clock ahead", () => {
  const serverOffset = 5000;
  const t0 = 0, t1 = serverOffset + 20, t2 = serverOffset + 20, t3 = 40;
  const { rttMs, offsetMs } = estimateClockOffset(t0, t1, t2, t3);
  assert.equal(rttMs, 40);
  assert.ok(Math.abs(offsetMs - serverOffset) < 1);
});

test("expectedPositionTicks: paused state does not advance", () => {
  const state = { positionTicks: 5 * TICKS_PER_SECOND, isPlaying: false, serverTimestampMs: 7000 };
  assert.equal(expectedPositionTicks(state, 10000, 0), 5 * TICKS_PER_SECOND);
});

test("expectedPositionTicks: playing state advances with elapsed time", () => {
  const state = { positionTicks: 10 * TICKS_PER_SECOND, isPlaying: true, serverTimestampMs: 0 };
  assert.equal(expectedPositionTicks(state, 3000, 0), 13 * TICKS_PER_SECOND);
});

test("expectedPositionTicks: applies clock offset", () => {
  const state = { positionTicks: 0, isPlaying: true, serverTimestampMs: 0 };
  // local clock reads 3000ms elapsed, but server is 2000ms ahead
  assert.equal(expectedPositionTicks(state, 3000, 2000), 5 * TICKS_PER_SECOND);
});

test("expectedPositionTicks: negative elapsed clamped to zero", () => {
  const state = { positionTicks: 20 * TICKS_PER_SECOND, isPlaying: true, serverTimestampMs: 5000 };
  assert.equal(expectedPositionTicks(state, 4900, 0), 20 * TICKS_PER_SECOND);
});

const thresholds = { softDriftMs: 300, hardDriftMs: 1500, maxRateAdjustment: 0.05 };

test("classifyDrift: within soft threshold => none", () => {
  const { action } = classifyDrift(10 * TICKS_PER_SECOND, 10 * TICKS_PER_SECOND + 100000, thresholds);
  assert.equal(action, DriftAction.NONE);
});

test("classifyDrift: soft boundary is exclusive", () => {
  const exactlyAtSoft = 300 * TICKS_PER_SECOND / 1000;
  const { action } = classifyDrift(exactlyAtSoft, 0, thresholds);
  assert.equal(action, DriftAction.NONE);
});

test("classifyDrift: between soft and hard => nudge", () => {
  const driftTicks = 600 * TICKS_PER_SECOND / 1000;
  const { action } = classifyDrift(driftTicks, 0, thresholds);
  assert.equal(action, DriftAction.NUDGE_RATE);
});

test("classifyDrift: beyond hard => hard seek", () => {
  const driftTicks = 2000 * TICKS_PER_SECOND / 1000;
  const { action } = classifyDrift(driftTicks, 0, thresholds);
  assert.equal(action, DriftAction.HARD_SEEK);
});

test("recommendedPlaybackRate: ahead slows down, behind speeds up", () => {
  const ahead = recommendedPlaybackRate(750 * TICKS_PER_SECOND / 1000, thresholds);
  const behind = recommendedPlaybackRate(-750 * TICKS_PER_SECOND / 1000, thresholds);
  assert.ok(ahead < 1.0);
  assert.ok(behind > 1.0);
});

test("recommendedPlaybackRate: clamped at max adjustment", () => {
  const farAhead = recommendedPlaybackRate(10000 * TICKS_PER_SECOND / 1000, thresholds);
  assert.ok(Math.abs(farAhead - (1.0 - 0.05)) < 1e-9);
});

test("recommendedPlaybackRate: exactly 1.0 at zero drift (returns to normal speed)", () => {
  assert.equal(recommendedPlaybackRate(0, thresholds), 1.0);
});

test("isStale: rejects older and equal sequence numbers, accepts newer", () => {
  assert.equal(isStale(5, 4), true);
  assert.equal(isStale(5, 5), true);
  assert.equal(isStale(5, 6), false);
});

test("isStale: delayed seek after a newer play is rejected", () => {
  // current state is already at seq 4 (a `play`); a delayed `seek` at seq 3
  // arrives afterward and must not be applied.
  assert.equal(isStale(4, 3), true);
});
