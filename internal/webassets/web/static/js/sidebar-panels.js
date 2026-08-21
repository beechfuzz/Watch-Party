// Pure state logic for the party page's right sidebar (Attendees /
// Playlist / Chat): collapse toggling, adjacent-pane resize math, and
// tolerant load/serialize for the persisted (localStorage) shape. Kept
// DOM-free and unit-testable the same way playlist-select.js/hls-source.js
// separate pure logic from player.js's DOM-heavy wiring -- see
// player.js's initSidebarPanels for the DOM/localStorage glue that calls
// into this module.

export const PANEL_KEYS = ["attendees", "playlist", "chat"];

// Arbitrary but reasonable starting split -- not derived from any
// measured container height, since flexbox's own shrink behavior (see
// .sidebar-block's flex-shrink:1 in style.css) scales these down
// proportionally if their sum ever exceeds the column's actual height.
const DEFAULT_HEIGHTS = { attendees: 180, playlist: 260, chat: 200 };

export const MIN_PANEL_HEIGHT = 80;

export function defaultPanelState() {
  const state = {};
  for (const key of PANEL_KEYS) state[key] = { collapsed: false, heightPx: DEFAULT_HEIGHTS[key] };
  return state;
}

// Tolerant of anything a previous, differently-shaped version of this
// might have stored, or of storage a user/extension mangled by hand:
// missing keys, non-object entries, and out-of-range values all just fall
// back to that key's default rather than throwing or producing a broken
// layout.
export function loadPanelState(raw) {
  const state = defaultPanelState();
  if (!raw) return state;
  let parsed;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return state;
  }
  if (!parsed || typeof parsed !== "object") return state;
  for (const key of PANEL_KEYS) {
    const entry = parsed[key];
    if (!entry || typeof entry !== "object") continue;
    if (typeof entry.collapsed === "boolean") state[key].collapsed = entry.collapsed;
    if (typeof entry.heightPx === "number" && entry.heightPx >= MIN_PANEL_HEIGHT) {
      state[key].heightPx = entry.heightPx;
    }
  }
  return state;
}

export function serializePanelState(state) {
  return JSON.stringify(state);
}

export function toggleCollapse(state, key) {
  return { ...state, [key]: { ...state[key], collapsed: !state[key].collapsed } };
}

// Adjacent-pane resize: dragging the handle between keyA (above) and keyB
// (below) by deltaY pixels trades height between just that pair -- the
// standard split-pane behavior, not a renegotiation across all panels.
// Clamped so neither drops below MIN_PANEL_HEIGHT; the pair's total height
// is always conserved exactly (see the tests), so clamping one side is
// guaranteed to leave the other still >= MIN_PANEL_HEIGHT as long as both
// started there, which every caller-supplied state satisfies (defaults
// and loadPanelState both only ever produce heights >= MIN_PANEL_HEIGHT).
// Callers must not call this with either panel collapsed -- there's
// nothing to negotiate against a fixed-height collapsed neighbor, so the
// DOM wiring disables that handle entirely rather than special-casing it
// here.
// lastExpandedKey returns whichever panel should absorb the column's
// leftover flex space: the last (bottom-most, per PANEL_KEYS order)
// currently-expanded panel, or null if every panel is collapsed (nothing
// to grow). Generalizes the old hardcoded "Chat always grows" rule -- Chat
// only ever worked as the grow target because it happened to always be the
// last block in the DOM *and* expanded; collapsing it reopens the identical
// gap under whatever is now the last expanded block (confirmed on Playlist,
// see ARCHITECTURE.md). Deriving this fresh from state on every call means
// there's no separate "which panel grows" field to keep in sync with
// collapse/expand -- it always falls out of {collapsed, heightPx} alone.
export function lastExpandedKey(state) {
  for (let i = PANEL_KEYS.length - 1; i >= 0; i--) {
    const key = PANEL_KEYS[i];
    if (!state[key].collapsed) return key;
  }
  return null;
}

export function resizePanels(state, keyA, keyB, deltaY) {
  const total = state[keyA].heightPx + state[keyB].heightPx;
  let newA = state[keyA].heightPx + deltaY;
  let newB = total - newA;
  if (newA < MIN_PANEL_HEIGHT) {
    newA = MIN_PANEL_HEIGHT;
    newB = total - newA;
  } else if (newB < MIN_PANEL_HEIGHT) {
    newB = MIN_PANEL_HEIGHT;
    newA = total - newB;
  }
  return {
    ...state,
    [keyA]: { ...state[keyA], heightPx: newA },
    [keyB]: { ...state[keyB], heightPx: newB },
  };
}
