// Pure predicate for the party page's native <video> event forwarding.
// Extracted from player.js so it's unit-testable under Node's test runner
// without a DOM, the same way sync.js/playlist-select.js separate pure
// logic from their DOM-heavy counterparts (see playlist-select.test.mjs).

/**
 * Whether a native "pause" event represents a real, user-initiated pause
 * that should be forwarded to the server as a host command -- as opposed to
 * the browser's own automatic pause when playback reaches the natural end
 * of the media (per the HTML Living Standard, reaching the end sets
 * `paused = true` and fires "pause" -- with `ended` already true -- before
 * firing "ended"). Forwarding that automatic pause as an authoritative
 * command was the root cause of a real bug: it set the server's IsPlaying
 * to false before the server's own end-of-media detection (checkEndOfMedia)
 * ever got a chance to run, permanently blocking auto-advance/pending-
 * transition for that item. The server is the sole authority for detecting
 * end-of-media (see SPEC.md's Synchronization Protocol section); this is
 * what keeps the client's native "pause" listener from second-guessing it.
 *
 * @param {{ended: boolean}} video
 */
export function isRealPause(video) {
  return !video.ended;
}
