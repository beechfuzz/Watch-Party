// Fullscreen chat overlay: a small, non-blocking card near the bottom-right
// of the video, shown when a chat message arrives while the viewer is in
// fullscreen, with a quick-reply input that sends through the exact same
// path as the sidebar composer (chat.js's sendChatMessage) rather than a
// second implementation. Trigger condition and truncation are pure logic
// in chat-format.js (shouldShowOverlay/truncateForOverlay); this module is
// the DOM/timer wiring around them, same split every other pair of pure/
// glue modules in this app uses.
//
// Dismiss behavior: auto-dismiss after AUTO_DISMISS_MS with no interaction;
// hovering the card or focusing the reply input pauses the timer for as
// long as that lasts (leaving restarts a fresh countdown, not a resumed
// partial one -- simpler, and arguably more correct: "you just looked away
// again" is a fresh read, not a continuation); a manual X always dismisses;
// a successful reply send dismisses immediately regardless of the timer.
// A newer incoming message replaces whatever's showing and resets the
// timer -- only one overlay is ever visible at a time (see .player-frame's
// :not(:fullscreen) CSS guard for the belt-and-suspenders "never renders
// outside fullscreen" rule).
import { sendChatMessage } from "./chat.js";
import { resolveIdentity } from "./avatar.js";
import { truncateForOverlay, shouldShowOverlay } from "./chat-format.js";

const AUTO_DISMISS_MS = 6000;

const overlayEl = document.getElementById("chat-overlay");
const dismissBtn = document.getElementById("chat-overlay-dismiss");
const senderEl = document.getElementById("chat-overlay-sender");
const textEl = document.getElementById("chat-overlay-text");
const replyForm = document.getElementById("chat-overlay-reply-form");
const replyInput = document.getElementById("chat-overlay-reply-input");

let meUserId = null;
let dismissTimer = null;
let pauseDepth = 0; // >0 while hovered and/or focused; only resume the timer once it drops back to 0

function clearDismissTimer() {
  if (dismissTimer) clearTimeout(dismissTimer);
  dismissTimer = null;
}

function armDismissTimer() {
  clearDismissTimer();
  if (pauseDepth > 0) return; // resumed later, when the last pause ends
  dismissTimer = setTimeout(dismissOverlay, AUTO_DISMISS_MS);
}

function dismissOverlay() {
  clearDismissTimer();
  overlayEl.classList.remove("is-visible");
}

function pause() {
  pauseDepth++;
  clearDismissTimer();
}

function resume() {
  pauseDepth = Math.max(0, pauseDepth - 1);
  if (pauseDepth === 0) armDismissTimer();
}

async function showOverlay(payload) {
  const identity = await resolveIdentity(payload.user_id);
  senderEl.textContent = identity.displayName;
  textEl.textContent = truncateForOverlay(payload.body);
  overlayEl.hidden = false;
  // Two-step (unhide, then add the transition class next frame) so the
  // slide+fade actually animates in -- toggling both at once wouldn't
  // transition from a starting state the browser ever painted.
  requestAnimationFrame(() => overlayEl.classList.add("is-visible"));
  armDismissTimer();
}

// handleChatMessageForOverlay is the extension point chat.js's initChat
// onMessage hook calls on every incoming chat_message -- checking
// document.fullscreenElement live (not cached) since fullscreen state can
// change between messages.
export function handleChatMessageForOverlay(payload) {
  if (!shouldShowOverlay(Boolean(document.fullscreenElement), payload.user_id, meUserId)) return;
  showOverlay(payload);
}

export function initChatOverlay({ meUserId: userId }) {
  meUserId = userId;

  dismissBtn.addEventListener("click", dismissOverlay);
  overlayEl.addEventListener("mouseenter", pause);
  overlayEl.addEventListener("mouseleave", resume);
  replyInput.addEventListener("focus", pause);
  replyInput.addEventListener("blur", resume);

  replyForm.addEventListener("submit", (e) => {
    e.preventDefault();
    if (sendChatMessage(replyInput.value)) {
      replyInput.value = "";
      dismissOverlay();
    }
  });

  // Leaving fullscreen entirely should not leave a stale card floating over
  // the now-windowed player.
  document.addEventListener("fullscreenchange", () => {
    if (!document.fullscreenElement) dismissOverlay();
  });
}
