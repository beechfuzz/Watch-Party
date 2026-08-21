// Run with: node --test internal/webassets/web/static/js/attendees.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import { visibleAttendees } from "./attendees.js";

function member(userID, { connected = true, isHost = false } = {}) {
  return {
    user_id: userID,
    display_name: userID,
    connection_status: connected ? "connected" : "disconnected",
    is_host: isHost,
  };
}

test("visibleAttendees: connected-only members pass through unchanged", () => {
  const members = [member("a"), member("b")];
  assert.deepEqual(visibleAttendees(members, false), members);
});

test("visibleAttendees: a disconnected non-host member is dropped", () => {
  const members = [member("a", { isHost: true }), member("b", { connected: false })];
  const result = visibleAttendees(members, false);
  assert.deepEqual(
    result.map((m) => m.user_id),
    ["a"],
  );
});

test("visibleAttendees: a member who explicitly left is dropped", () => {
  const left = { user_id: "b", display_name: "b", connection_status: "left", is_host: false };
  const members = [member("a", { isHost: true }), left];
  const result = visibleAttendees(members, false);
  assert.deepEqual(
    result.map((m) => m.user_id),
    ["a"],
  );
});

test("visibleAttendees: a disconnected host is kept and pinned first when hostReconnecting is true", () => {
  const members = [
    member("a"),
    member("host", { connected: false, isHost: true }),
    member("b"),
  ];
  const result = visibleAttendees(members, true);
  assert.deepEqual(
    result.map((m) => m.user_id),
    ["host", "a", "b"],
  );
});

test("visibleAttendees: a disconnected host is dropped once hostReconnecting is false", () => {
  const members = [member("a"), member("host", { connected: false, isHost: true })];
  const result = visibleAttendees(members, false);
  assert.deepEqual(
    result.map((m) => m.user_id),
    ["a"],
  );
});

test("visibleAttendees: a connected host not first in the input is still sorted to the front", () => {
  const members = [member("a"), member("b"), member("host", { isHost: true })];
  const result = visibleAttendees(members, false);
  assert.deepEqual(
    result.map((m) => m.user_id),
    ["host", "a", "b"],
  );
});

test("visibleAttendees: empty input returns empty output", () => {
  assert.deepEqual(visibleAttendees([], false), []);
  assert.deepEqual(visibleAttendees([], true), []);
});

test("visibleAttendees: no host in the input leaves relative order unchanged", () => {
  const members = [member("a"), member("b"), member("c")];
  assert.deepEqual(visibleAttendees(members, true), members);
});
