// Run with: node --test internal/webassets/web/static/js/sidebar-panels.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import {
  PANEL_KEYS,
  MIN_PANEL_HEIGHT,
  defaultPanelState,
  loadPanelState,
  serializePanelState,
  toggleCollapse,
  resizePanels,
  lastExpandedKey,
} from "./sidebar-panels.js";

test("defaultPanelState: every panel starts expanded with a height at or above the minimum", () => {
  const state = defaultPanelState();
  for (const key of PANEL_KEYS) {
    assert.equal(state[key].collapsed, false);
    assert.ok(state[key].heightPx >= MIN_PANEL_HEIGHT);
  }
});

test("loadPanelState: falls back to defaults when nothing is stored", () => {
  assert.deepEqual(loadPanelState(null), defaultPanelState());
  assert.deepEqual(loadPanelState(""), defaultPanelState());
});

test("loadPanelState: falls back to defaults on malformed JSON", () => {
  assert.deepEqual(loadPanelState("{not valid json"), defaultPanelState());
});

test("loadPanelState: falls back to defaults when the stored value isn't an object", () => {
  assert.deepEqual(loadPanelState("42"), defaultPanelState());
  assert.deepEqual(loadPanelState('"a string"'), defaultPanelState());
});

test("loadPanelState: merges partial stored state over defaults, ignoring unknown/malformed entries", () => {
  const raw = JSON.stringify({
    playlist: { collapsed: true, heightPx: 300 },
    chat: { collapsed: "not a boolean", heightPx: 50 }, // bad types/out-of-range -> ignored per-field
    "unknown-key": { collapsed: true, heightPx: 999 },
  });
  const state = loadPanelState(raw);
  const defaults = defaultPanelState();

  assert.equal(state["watching-now"].collapsed, defaults["watching-now"].collapsed);
  assert.equal(state["watching-now"].heightPx, defaults["watching-now"].heightPx);

  assert.equal(state.playlist.collapsed, true);
  assert.equal(state.playlist.heightPx, 300);

  // chat.collapsed was a string, not a boolean -> kept at default; heightPx
  // was below MIN_PANEL_HEIGHT -> also kept at default.
  assert.equal(state.chat.collapsed, defaults.chat.collapsed);
  assert.equal(state.chat.heightPx, defaults.chat.heightPx);

  assert.equal(state["unknown-key"], undefined);
});

test("serializePanelState + loadPanelState round-trip", () => {
  const state = toggleCollapse(defaultPanelState(), "chat");
  const round = loadPanelState(serializePanelState(state));
  assert.deepEqual(round, state);
});

test("toggleCollapse: flips only the targeted panel's collapsed flag", () => {
  const state = defaultPanelState();
  const next = toggleCollapse(state, "playlist");
  assert.equal(next.playlist.collapsed, true);
  assert.equal(next["watching-now"].collapsed, false);
  assert.equal(next.chat.collapsed, false);
  // original state object is untouched (pure function)
  assert.equal(state.playlist.collapsed, false);

  const back = toggleCollapse(next, "playlist");
  assert.equal(back.playlist.collapsed, false);
});

test("resizePanels: trades height between the dragged pair, conserving their total", () => {
  const state = defaultPanelState();
  const totalBefore = state["watching-now"].heightPx + state.playlist.heightPx;

  const next = resizePanels(state, "watching-now", "playlist", 40);
  assert.equal(next["watching-now"].heightPx, state["watching-now"].heightPx + 40);
  assert.equal(next.playlist.heightPx, state.playlist.heightPx - 40);
  assert.equal(next["watching-now"].heightPx + next.playlist.heightPx, totalBefore);

  // the third panel is untouched
  assert.equal(next.chat.heightPx, state.chat.heightPx);
});

test("resizePanels: clamps the shrinking panel at MIN_PANEL_HEIGHT and gives the rest to its neighbor", () => {
  const state = defaultPanelState(); // watching-now=180, playlist=260
  const total = state["watching-now"].heightPx + state.playlist.heightPx;

  // Drag far enough negative that watching-now would go below the minimum.
  const next = resizePanels(state, "watching-now", "playlist", -1000);
  assert.equal(next["watching-now"].heightPx, MIN_PANEL_HEIGHT);
  assert.equal(next.playlist.heightPx, total - MIN_PANEL_HEIGHT);
});

test("resizePanels: clamps the growing panel's neighbor at MIN_PANEL_HEIGHT symmetrically", () => {
  const state = defaultPanelState();
  const total = state["watching-now"].heightPx + state.playlist.heightPx;

  const next = resizePanels(state, "watching-now", "playlist", 1000);
  assert.equal(next.playlist.heightPx, MIN_PANEL_HEIGHT);
  assert.equal(next["watching-now"].heightPx, total - MIN_PANEL_HEIGHT);
});

test("resizePanels: is a pure function -- does not mutate its input state", () => {
  const state = defaultPanelState();
  const before = JSON.stringify(state);
  resizePanels(state, "playlist", "chat", 25);
  assert.equal(JSON.stringify(state), before);
});

test("lastExpandedKey: with nothing collapsed, returns the last panel in PANEL_KEYS order", () => {
  assert.equal(lastExpandedKey(defaultPanelState()), "chat");
});

test("lastExpandedKey: collapsing the last panel hands the grow target to the new last-expanded one", () => {
  // The exact regression this generalizes: collapsing Chat used to reopen
  // the leftover-space gap under Playlist, since only Chat ever grew.
  const state = toggleCollapse(defaultPanelState(), "chat");
  assert.equal(lastExpandedKey(state), "playlist");
});

test("lastExpandedKey: collapsing the last two panels hands the grow target to watching-now", () => {
  let state = toggleCollapse(defaultPanelState(), "chat");
  state = toggleCollapse(state, "playlist");
  assert.equal(lastExpandedKey(state), "watching-now");
});

test("lastExpandedKey: re-expanding a panel gives it back the grow target, since it's re-derived, not sticky", () => {
  let state = toggleCollapse(defaultPanelState(), "chat");
  assert.equal(lastExpandedKey(state), "playlist");
  state = toggleCollapse(state, "chat"); // re-expand
  assert.equal(lastExpandedKey(state), "chat");
});

test("lastExpandedKey: returns null when every panel is collapsed", () => {
  let state = defaultPanelState();
  for (const key of PANEL_KEYS) state = toggleCollapse(state, key);
  assert.equal(lastExpandedKey(state), null);
});
