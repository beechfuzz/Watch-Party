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
import { api, clearCSRFToken } from "./api.js";

export function initSidebar({ onBeforeNavigate, onLoggedOut } = {}) {
  const avatarInitial = document.getElementById("user-avatar-initial");
  const userName = document.getElementById("user-name");
  const logoutBtn = document.getElementById("logout-btn");

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
      avatarInitial.textContent = initials(me.user.display_name);
      userName.textContent = me.user.display_name;
    },
  };
}

function initials(name) {
  return (name || "?").trim().slice(0, 1).toUpperCase();
}
