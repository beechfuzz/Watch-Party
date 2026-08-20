// Pure logic for party chat: message-body validation (mirrors the server's
// own length check so a doomed send never has to round-trip to find out)
// and overlay truncation. Kept DOM/network-free and unit-testable, the same
// split playlist-select.js/sidebar-panels.js already established between
// pure logic and player.js's/chat.js's DOM-heavy wiring.

// Kept in sync with internal/party.MaxChatMessageLength by convention, not
// a shared build step -- this project has none (see CLAUDE.md's tech
// stack), the same way the sync math in sync.js mirrors internal/syncalg
// rather than sharing it.
export const MAX_CHAT_MESSAGE_LENGTH = 300;

// Matches player.js's own local post-send throttle: after a user sends a
// message, their own input is disabled for this long -- a local UX
// courtesy, not a substitute for the server's own rate limiting.
export const POST_SEND_DISABLE_MS = 500;

// validateChatBody mirrors the server's own trim/empty/length checks
// (internal/party.HandleChatSend) so an obviously-doomed send (empty, or
// over length) never has to round-trip to the server to find out. The
// server re-validates independently regardless -- this is a client-side
// courtesy, not the actual enforcement boundary.
//
// Length is measured the same way a JS string's .length already does
// (UTF-16 code units), which is what a client <input maxlength> enforces
// too -- close enough to the server's rune count for everyday text; wildly
// exotic multi-code-unit graphemes could in principle count slightly
// differently between the two, which is fine, since the server's count is
// the one that actually matters.
export function validateChatBody(raw) {
  const trimmed = (raw || "").trim();
  if (trimmed === "") return { ok: false, reason: "empty" };
  if (trimmed.length > MAX_CHAT_MESSAGE_LENGTH) return { ok: false, reason: "too_long" };
  return { ok: true, body: trimmed };
}

// truncateForOverlay returns the first maxLen characters of body, followed
// by "…" if it was longer -- used by the fullscreen quick-reply overlay
// (see the party-chat-overlay PR) for its "first 50 characters" preview.
// Exported from here (not a fullscreen-specific module) since it's pure
// text logic with no dependency on fullscreen/DOM state.
export function truncateForOverlay(body, maxLen = 50) {
  if (body.length <= maxLen) return body;
  return body.slice(0, maxLen) + "…";
}
