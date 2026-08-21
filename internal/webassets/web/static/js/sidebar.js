// Shared left-hand nav wiring, reused by both page entry points (app.js on
// the dashboard, player.js on the party page) against the one markup copy
// they both now render via the "sidebar" template partial
// (_sidebar.html) -- see ARCHITECTURE.md's Persistent sidebar section.
//
// Wired once per page at module load (like every other top-level listener
// in app.js/player.js), not re-wired per render -- the caller updates the
// displayed user via the returned setUser() once its own /api/me call
// resolves, instead of initSidebar taking `me` directly and risking
// duplicate listeners if the caller's render path ever runs more than once
// (e.g. app.js's showHome, reachable again after a same-page logout/login
// cycle with no reload in between).
//
// onBeforeNavigate, when supplied, is awaited before following a sidebar
// link or signing out -- this is what makes "navigate away from a party
// via the sidebar" register as an explicit leave rather than a silent
// disconnect. The dashboard has nothing to leave, so app.js calls
// initSidebar without it and a sidebar click just behaves like a normal
// link/logout.
//
// This module also owns the sidebar's collapsed/expanded (icon-only rail)
// toggle -- pure load/serialize logic lives in sidebar-collapse.js, unit
// tested there without a DOM, the same split sidebar-panels.js/player.js
// use for the right sidebar's collapse/resize state.
import { api, clearCSRFToken } from "./api.js";
import { loadCollapsedState, serializeCollapsedState } from "./sidebar-collapse.js";
import { resolveIdentity, renderAvatar } from "./avatar.js";

const COLLAPSE_STORAGE_KEY = "watchparty:sidebarCollapsed";

// Applied synchronously, before any of initSidebar's async callers resolve
// (both app.js and player.js call initSidebar() at module-eval time), so a
// returning visitor's persisted collapsed state is on screen at first paint
// -- the same early-apply timing player.js's initSidebarPanels uses for the
// right sidebar's persisted state.
function applyCollapsedState(sidebarEl, toggleBtn, collapsed) {
  sidebarEl.classList.toggle("is-collapsed", collapsed);
  toggleBtn.setAttribute("aria-expanded", String(!collapsed));
  toggleBtn.setAttribute("aria-label", collapsed ? "Expand sidebar" : "Collapse sidebar");
}

export function initSidebar({ onBeforeNavigate, onLoggedOut } = {}) {
  const avatarInitial = document.getElementById("user-avatar-initial");
  const userName = document.getElementById("user-name");
  const logoutBtn = document.getElementById("logout-btn");
  const sidebarEl = document.querySelector(".sidebar");
  const collapseToggleBtn = document.getElementById("sidebar-collapse-toggle");

  let collapsed = loadCollapsedState(localStorage.getItem(COLLAPSE_STORAGE_KEY));
  applyCollapsedState(sidebarEl, collapseToggleBtn, collapsed);
  collapseToggleBtn.addEventListener("click", () => {
    collapsed = !collapsed;
    applyCollapsedState(sidebarEl, collapseToggleBtn, collapsed);
    localStorage.setItem(COLLAPSE_STORAGE_KEY, serializeCollapsedState(collapsed));
  });

  for (const link of document.querySelectorAll(".nav-item[href]")) {
    link.addEventListener("click", async (e) => {
      if (!onBeforeNavigate) return; // plain link, default navigation is fine
      e.preventDefault();
      await onBeforeNavigate();
      window.location.href = link.href;
    });
  }

  logoutBtn.addEventListener("click", async () => {
    if (onBeforeNavigate) await onBeforeNavigate();
    try {
      await api("/api/auth/logout", { method: "POST" });
    } finally {
      clearCSRFToken();
      if (onLoggedOut) onLoggedOut();
    }
  });

  return {
    setUser(me) {
      // Own-avatar lookup: the same GET /api/emby/users/{id} resolution
      // every other avatar in this app uses, just with the target set to
      // the signed-in user's own id -- a user reading their own Emby
      // profile is always permitted, so this isn't a special case for the
      // backend (see ARCHITECTURE.md's Party chat section). Shares
      // avatar.js's page-wide cache, so this costs a fresh round trip only
      // if nothing else on the page already resolved this same user.
      renderAvatar(avatarInitial, { avatarUrl: "", displayName: me.user.display_name });
      resolveIdentity(me.user.id).then((identity) => {
        renderAvatar(avatarInitial, identity);
      });
      userName.textContent = me.user.display_name;
    },
  };
}
