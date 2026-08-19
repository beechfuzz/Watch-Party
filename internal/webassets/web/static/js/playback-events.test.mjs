// Run with: node --test internal/webassets/web/static/js/playback-events.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import { isRealPause } from "./playback-events.js";

test("isRealPause: false when the video has reached its natural end (the browser's own automatic pause)", () => {
  assert.equal(isRealPause({ ended: true }), false);
});

test("isRealPause: true for a genuine user-initiated pause mid-playback", () => {
  assert.equal(isRealPause({ ended: false }), true);
});
