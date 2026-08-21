// Pure filter/ordering logic for the party page's Attendees list — kept
// DOM-free so it's unit-testable with node --test (this project has no
// jsdom; see ARCHITECTURE.md's Attendees section for why the rendered-DOM
// behavior of this list is instead verified against a real browser,
// separately, following the same split established by playlist-select.js /
// sidebar-panels.js / hls-source.js / playback-events.js).
//
// A member is shown only if currently connected, with one pinned exception:
// the host, while disconnected but still within the live host-authority
// grace period (hostReconnecting). Any other disconnected/left member is
// dropped entirely rather than shown dimmed. The host (connected or
// reconnecting) is always sorted first.
export function visibleAttendees(members, hostReconnecting) {
  const visible = members.filter(
    (m) => m.connection_status === "connected" || (m.is_host && hostReconnecting)
  );
  const hostIdx = visible.findIndex((m) => m.is_host);
  if (hostIdx > 0) {
    const [host] = visible.splice(hostIdx, 1);
    visible.unshift(host);
  }
  return visible;
}
