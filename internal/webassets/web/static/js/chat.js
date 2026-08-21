// Party page: the Chat sidebar block -- message list, composer, chat
// history load, and per-sender identity resolution. Validation/truncation
// logic lives in chat-format.js as pure functions; this module is the DOM/
// network wiring around it, the same split playlist.js/playlist-select.js
// already established.
//
// Sender identity (display name + avatar) is deliberately never part of a
// chat message's own payload -- it's resolved live, per the *viewing*
// participant's own Emby token, via GET /api/emby/users/{id} (see
// ARCHITECTURE.md's Party chat section) -- the same per-viewer resolution
// pattern already used for playlist item titles/posters. Resolution,
// caching, and avatar-vs-initials rendering now live in avatar.js, shared
// with the sidebar's own avatar, the dashboard's host rows, and the party
// room's Watching Now list, so a repeat sender (in any of those places, not
// just chat) costs one Emby round trip, not one per appearance.
import { api } from "./api.js";
import { validateChatBody, POST_SEND_DISABLE_MS } from "./chat-format.js";
import { resolveIdentity, renderAvatar } from "./avatar.js";

const messagesEl = document.getElementById("chat-messages");
const inputEl = document.getElementById("chat-input");
const sendBtn = document.getElementById("chat-send-btn");

let partyId = null;
let meUserId = null;
let sendFn = () => {};
let onMessageFn = () => {}; // extension point for the fullscreen overlay (added by a later PR)

function isScrolledToBottom() {
  return messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight < 40;
}

function clearEmptyState() {
  const empty = messagesEl.querySelector(".chat-empty");
  if (empty) empty.remove();
}

async function renderMessage(msg) {
  clearEmptyState();
  const wasAtBottom = isScrolledToBottom();

  const row = document.createElement("div");
  row.className = `chat-message${msg.user_id === meUserId ? " is-own" : ""}`;
  row.innerHTML = `
    <div class="chat-message-avatar"></div>
    <div class="chat-message-body">
      <div class="chat-message-sender"></div>
      <div class="chat-message-text"></div>
    </div>
  `;
  row.querySelector(".chat-message-text").textContent = msg.body;
  messagesEl.appendChild(row);
  if (wasAtBottom) messagesEl.scrollTop = messagesEl.scrollHeight;

  const identity = await resolveIdentity(msg.user_id);
  // The row may have scrolled out or the panel may have re-rendered by the
  // time the identity lookup resolves -- guard against a detached node.
  if (!row.isConnected) return;
  row.querySelector(".chat-message-sender").textContent = identity.displayName;
  renderAvatar(row.querySelector(".chat-message-avatar"), identity);
}

async function loadHistory() {
  let res;
  try {
    res = await api(`/api/parties/${encodeURIComponent(partyId)}/chat`);
  } catch {
    return; // best-effort -- an empty/failed history load isn't fatal to the page
  }
  for (const msg of res.messages || []) {
    await renderMessage(msg);
  }
}

function setComposerDisabled(disabled) {
  inputEl.disabled = disabled;
  sendBtn.disabled = disabled;
}

// send() is exported so the fullscreen quick-reply overlay (a later PR)
// can submit through the exact same validation/throttle/transport path as
// the sidebar composer, rather than duplicating any of this.
export function sendChatMessage(rawBody) {
  const result = validateChatBody(rawBody);
  if (!result.ok) return false;
  sendFn(result.body);
  setComposerDisabled(true);
  setTimeout(() => setComposerDisabled(false), POST_SEND_DISABLE_MS);
  return true;
}

// handleIncomingChatMessage is called from player.js's WS dispatch on every
// "chat_message" broadcast -- including the sender's own, since this app's
// clients never optimistically render their own action locally, only ever
// the server's broadcast (the same "react to the broadcast, not the local
// action" discipline the sync engine itself uses).
export function handleIncomingChatMessage(payload) {
  renderMessage(payload);
  onMessageFn(payload);
}

// initChat wires the composer and kicks off the history load. send is a
// thin callback so this module doesn't need to know about the WebSocket
// connection itself (owned by player.js) -- e.g. `(body) => conn.send("chat_send", { body })`.
// onMessage, if given, is called with every incoming chat_message payload
// after it's rendered -- the extension point the fullscreen overlay PR
// hooks into, so this module doesn't need to know fullscreen exists.
export function initChat({ partyId: id, meUserId: userId, send, onMessage }) {
  partyId = id;
  meUserId = userId;
  sendFn = send;
  if (onMessage) onMessageFn = onMessage;

  const trySend = () => {
    if (sendChatMessage(inputEl.value)) inputEl.value = "";
  };
  sendBtn.addEventListener("click", trySend);
  inputEl.addEventListener("keydown", (e) => {
    if (e.key === "Enter") trySend();
  });

  loadHistory();
}
