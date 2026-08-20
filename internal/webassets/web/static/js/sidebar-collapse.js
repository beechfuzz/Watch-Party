// Pure state logic for the left sidebar's collapsed/expanded toggle. Kept
// DOM-free and unit-testable the same way sidebar-panels.js separates the
// right sidebar's pure state logic from player.js's DOM-heavy wiring -- see
// sidebar.js's initSidebar for the DOM/localStorage glue that calls into
// this module.
//
// Deliberately smaller than sidebar-panels.js's PANEL_KEYS-shaped state:
// there's exactly one boolean to persist here, so this skips a dedicated
// toggle() wrapper -- negating a boolean doesn't earn a named function the
// way sidebar-panels.js's multi-key toggleCollapse does.

export function defaultCollapsedState() {
  return false;
}

// Tolerant of anything a previous version might have stored, or of storage
// a user/extension mangled by hand: anything other than the exact strings
// this module itself writes falls back to the default instead of throwing
// or producing a broken layout.
export function loadCollapsedState(raw) {
  if (raw === "true") return true;
  if (raw === "false") return false;
  return defaultCollapsedState();
}

export function serializeCollapsedState(collapsed) {
  return collapsed ? "true" : "false";
}
