// Run with: node --test internal/webassets/web/static/js/chat-format.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import { MAX_CHAT_MESSAGE_LENGTH, validateChatBody, truncateForOverlay, shouldShowOverlay } from "./chat-format.js";

test("validateChatBody: rejects empty and whitespace-only input", () => {
  for (const raw of ["", "   ", "\t\n", undefined, null]) {
    const result = validateChatBody(raw);
    assert.equal(result.ok, false);
    assert.equal(result.reason, "empty");
  }
});

test("validateChatBody: trims surrounding whitespace on success", () => {
  const result = validateChatBody("  hello world  ");
  assert.equal(result.ok, true);
  assert.equal(result.body, "hello world");
});

test("validateChatBody: accepts a message exactly at the max length", () => {
  const body = "a".repeat(MAX_CHAT_MESSAGE_LENGTH);
  const result = validateChatBody(body);
  assert.equal(result.ok, true);
  assert.equal(result.body, body);
});

test("validateChatBody: rejects a message one character over the max length", () => {
  const result = validateChatBody("a".repeat(MAX_CHAT_MESSAGE_LENGTH + 1));
  assert.equal(result.ok, false);
  assert.equal(result.reason, "too_long");
});

test("validateChatBody: length check applies after trimming, not before", () => {
  // Padding a message out with whitespace to sneak past the trimmed limit
  // must not work -- the check has to run on the trimmed body.
  const body = "a".repeat(MAX_CHAT_MESSAGE_LENGTH) + "   ";
  const result = validateChatBody(body);
  assert.equal(result.ok, true);
  assert.equal(result.body.length, MAX_CHAT_MESSAGE_LENGTH);
});

test("truncateForOverlay: returns the body unchanged when at or under the limit", () => {
  assert.equal(truncateForOverlay("short message"), "short message");
  assert.equal(truncateForOverlay("a".repeat(50), 50), "a".repeat(50));
});

test("truncateForOverlay: truncates to exactly maxLen characters plus an ellipsis", () => {
  const body = "a".repeat(60);
  const result = truncateForOverlay(body, 50);
  assert.equal(result, "a".repeat(50) + "…");
  assert.equal(result.length, 51);
});

test("truncateForOverlay: default maxLen is 50", () => {
  const body = "a".repeat(51);
  assert.equal(truncateForOverlay(body), "a".repeat(50) + "…");
});

test("shouldShowOverlay: shown while fullscreen for another user's message", () => {
  assert.equal(shouldShowOverlay(true, "user-bob", "user-alice"), true);
});

test("shouldShowOverlay: not shown when not fullscreen, even for another user's message", () => {
  assert.equal(shouldShowOverlay(false, "user-bob", "user-alice"), false);
});

test("shouldShowOverlay: not shown for the viewer's own message, even while fullscreen", () => {
  assert.equal(shouldShowOverlay(true, "user-alice", "user-alice"), false);
});

test("shouldShowOverlay: not shown when neither condition holds", () => {
  assert.equal(shouldShowOverlay(false, "user-alice", "user-alice"), false);
});
