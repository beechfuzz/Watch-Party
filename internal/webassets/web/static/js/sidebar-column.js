// Pure state logic for the party page's right sidebar *column* as a whole
// (Attendees / Playlist / Chat together): horizontal width resize and a
// collapse toggle for the column, distinct from -- and independent of --
// sidebar-panels.js's per-block vertical height/collapse state. Kept
// DOM-free and unit-testable the same way sidebar-panels.js/
// sidebar-collapse.js separate pure logic from player.js's DOM-heavy
// wiring -- see player.js's initSidebarColumn for the DOM/localStorage
// glue that calls into this module.
//
// This module never reads or writes sidebar-panels.js's state (per-block
// {collapsed, heightPx}) or sidebar-collapse.js's state (the left
// sidebar's boolean) -- three separate modules, three separate
// localStorage keys, on purpose.

export const DEFAULT_SIDEBAR_WIDTH = 300;
export const MIN_SIDEBAR_WIDTH = 220;
export const MAX_SIDEBAR_WIDTH = 560;

export function defaultColumnState() {
  return { collapsed: false, widthPx: DEFAULT_SIDEBAR_WIDTH };
}

function clampWidth(widthPx) {
  return Math.min(MAX_SIDEBAR_WIDTH, Math.max(MIN_SIDEBAR_WIDTH, widthPx));
}

// Tolerant of anything a previous version might have stored, or of storage
// a user/extension mangled by hand: missing keys, non-object entries, and
// out-of-range values all just fall back to that key's default rather than
// throwing or producing a broken layout -- same posture as
// sidebar-panels.js's loadPanelState/sidebar-collapse.js's
// loadCollapsedState.
export function loadColumnState(raw) {
  const state = defaultColumnState();
  if (!raw) return state;
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return state;
  }
  if (!parsed || typeof parsed !== "object") return state;
  if (typeof parsed.collapsed === "boolean") state.collapsed = parsed.collapsed;
  if (typeof parsed.widthPx === "number" && parsed.widthPx >= MIN_SIDEBAR_WIDTH && parsed.widthPx <= MAX_SIDEBAR_WIDTH) {
    state.widthPx = parsed.widthPx;
  }
  return state;
}

export function serializeColumnState(state) {
  return JSON.stringify(state);
}

export function toggleColumnCollapse(state) {
  return { ...state, collapsed: !state.collapsed };
}

// Dragging the handle on the column's left edge left (negative deltaX)
// widens the column; dragging right narrows it. widthPx is kept clamped
// to [MIN_SIDEBAR_WIDTH, MAX_SIDEBAR_WIDTH] at all times, including while
// collapsed, so the column reopens at whatever width it had before --
// same "heightPx survives collapse" precedent sidebar-panels.js's
// toggleCollapse already establishes for the per-block state.
export function resizeColumn(state, deltaX) {
  return { ...state, widthPx: clampWidth(state.widthPx - deltaX) };
}
