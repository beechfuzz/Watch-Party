import { api } from "./api.js";

// Shared account-avatar resolution and rendering, used everywhere an
// account's avatar appears: the sidebar's own-user avatar (sidebar.js), the
// dashboard's per-party host row (app.js), the party room's Attendees
// list (player.js), and party chat (chat.js). All four resolve the same
// way -- GET /api/emby/users/{id}, using the *viewing* participant's own
// Emby token, never a shared/service token or the target's own token (see
// ARCHITECTURE.md's Party chat section, which established this pattern for
// chat sender identity first). This module generalizes what chat.js
// originally built one-off, so every consumer shares one cache instead of
// each duplicating its own fetch/memoize/fallback logic.
//
// createAvatarResolver takes the network call as an injected parameter
// specifically so the memoize/in-flight-dedup/fallback behavior can be
// unit-tested (avatar.test.mjs) without a DOM or a real fetch -- the same
// reason other pure-logic modules in this app (chat-format.js,
// playlist-select.js) stay dependency-free.
export function createAvatarResolver(fetchUser) {
  // userId -> { displayName, avatarUrl } | Promise<...>. A pending Promise
  // is stored (not just the eventual result) so two lookups for the same
  // unresolved id arriving close together share one in-flight request
  // instead of firing a duplicate each -- ported as-is from chat.js's
  // original identityCache.
  const cache = new Map();

  function resolve(userId) {
    if (cache.has(userId)) return cache.get(userId);
    const promise = fetchUser(userId)
      .then((res) => ({ displayName: res.display_name || userId, avatarUrl: res.avatar_url || "" }))
      .catch(() => ({ displayName: userId, avatarUrl: "" }));
    cache.set(userId, promise);
    return promise;
  }

  return { resolve };
}

// The one shared instance every DOM module imports, so a user who appears
// in more than one place on the same page (e.g. a party member who's also
// sent a chat message, or a host running more than one visible party) costs
// one /api/emby/users/{id} round trip for the page's lifetime, not one per
// call site.
export const resolveIdentity = createAvatarResolver((userId) => api(`/api/emby/users/${encodeURIComponent(userId)}`)).resolve;

export function initials(name) {
  return (name || "?").trim().slice(0, 1).toUpperCase();
}

// renderAvatar fills el with either a real profile picture or the
// initials fallback. Used for both a plain "whole element is the avatar"
// container (host-row/participant-row/chat-message-avatar's .avatar div) and
// an inner initial-holder element (sidebar's #user-avatar-initial span,
// which has to stay a sibling of the .status-dot indicator rather than
// having its parent's markup replaced outright).
//
// Falls back to initials, not a broken image, if the URL itself fails to
// load in the browser (a stale/invalid api_key, a since-deleted image,
// etc.) -- avatarUrl having been non-empty here only means Emby reported a
// PrimaryImageTag at resolve time, not that the image is guaranteed to
// still load now.
export function renderAvatar(el, { avatarUrl, displayName }) {
  if (avatarUrl) {
    el.innerHTML = `<img src="${avatarUrl}" alt="">`;
    el.querySelector("img").addEventListener("error", () => {
      el.textContent = initials(displayName);
    });
  } else {
    el.textContent = initials(displayName);
  }
}
