// Run with: node --test internal/webassets/web/static/js/sidebar-column.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import {
  DEFAULT_SIDEBAR_WIDTH,
  MIN_SIDEBAR_WIDTH,
  MAX_SIDEBAR_WIDTH,
  defaultColumnState,
  loadColumnState,
  serializeColumnState,
  toggleColumnCollapse,
  resizeColumn,
} from "./sidebar-column.js";

test("defaultColumnState: starts expanded at the default width", () => {
  const state = defaultColumnState();
  assert.equal(state.collapsed, false);
  assert.equal(state.widthPx, DEFAULT_SIDEBAR_WIDTH);
});

test("loadColumnState: falls back to defaults when nothing is stored", () => {
  assert.deepEqual(loadColumnState(null), defaultColumnState());
  assert.deepEqual(loadColumnState(""), defaultColumnState());
  assert.deepEqual(loadColumnState(undefined), defaultColumnState());
});

test("loadColumnState: falls back to defaults on malformed JSON", () => {
  assert.deepEqual(loadColumnState("{not valid json"), defaultColumnState());
});

test("loadColumnState: falls back to defaults when the stored value isn't an object", () => {
  assert.deepEqual(loadColumnState("42"), defaultColumnState());
  assert.deepEqual(loadColumnState('"a string"'), defaultColumnState());
});

test("loadColumnState: reads back a validly stored value", () => {
  const raw = JSON.stringify({ collapsed: true, widthPx: 400 });
  const state = loadColumnState(raw);
  assert.equal(state.collapsed, true);
  assert.equal(state.widthPx, 400);
});

test("loadColumnState: ignores a malformed collapsed field, keeping the default", () => {
  const raw = JSON.stringify({ collapsed: "not a boolean", widthPx: 400 });
  const state = loadColumnState(raw);
  assert.equal(state.collapsed, defaultColumnState().collapsed);
  assert.equal(state.widthPx, 400);
});

test("loadColumnState: ignores an out-of-range widthPx, keeping the default", () => {
  const tooNarrow = loadColumnState(JSON.stringify({ collapsed: false, widthPx: MIN_SIDEBAR_WIDTH - 1 }));
  assert.equal(tooNarrow.widthPx, DEFAULT_SIDEBAR_WIDTH);

  const tooWide = loadColumnState(JSON.stringify({ collapsed: false, widthPx: MAX_SIDEBAR_WIDTH + 1 }));
  assert.equal(tooWide.widthPx, DEFAULT_SIDEBAR_WIDTH);
});

test("serializeColumnState + loadColumnState round-trip", () => {
  const state = resizeColumn(defaultColumnState(), -50);
  const round = loadColumnState(serializeColumnState(state));
  assert.deepEqual(round, state);
});

test("toggleColumnCollapse: flips collapsed without touching widthPx", () => {
  const state = defaultColumnState();
  const next = toggleColumnCollapse(state);
  assert.equal(next.collapsed, true);
  assert.equal(next.widthPx, state.widthPx);
  // original state object is untouched (pure function)
  assert.equal(state.collapsed, false);

  const back = toggleColumnCollapse(next);
  assert.equal(back.collapsed, false);
  assert.equal(back.widthPx, state.widthPx);
});

test("resizeColumn: dragging left (negative deltaX) widens the column", () => {
  const state = defaultColumnState();
  const next = resizeColumn(state, -40);
  assert.equal(next.widthPx, state.widthPx + 40);
});

test("resizeColumn: dragging right (positive deltaX) narrows the column", () => {
  const state = defaultColumnState();
  const next = resizeColumn(state, 40);
  assert.equal(next.widthPx, state.widthPx - 40);
});

test("resizeColumn: clamps at MIN_SIDEBAR_WIDTH", () => {
  const state = defaultColumnState();
  const next = resizeColumn(state, 10000);
  assert.equal(next.widthPx, MIN_SIDEBAR_WIDTH);
});

test("resizeColumn: clamps at MAX_SIDEBAR_WIDTH", () => {
  const state = defaultColumnState();
  const next = resizeColumn(state, -10000);
  assert.equal(next.widthPx, MAX_SIDEBAR_WIDTH);
});

test("resizeColumn: is a pure function -- does not mutate its input state", () => {
  const state = defaultColumnState();
  const before = JSON.stringify(state);
  resizeColumn(state, 25);
  assert.equal(JSON.stringify(state), before);
});

test("resizeColumn: widthPx keeps clamping correctly even while collapsed, so it survives to reopen at", () => {
  let state = toggleColumnCollapse(defaultColumnState());
  assert.equal(state.collapsed, true);
  state = resizeColumn(state, -60);
  assert.equal(state.widthPx, DEFAULT_SIDEBAR_WIDTH + 60);
  state = toggleColumnCollapse(state);
  assert.equal(state.collapsed, false);
  assert.equal(state.widthPx, DEFAULT_SIDEBAR_WIDTH + 60);
});
