// Index page: login form, then the dashboard (active/your parties +
// create). Server is the source of truth; this just renders whatever
// /api/me and /api/parties say.
//
// home-section and create-party-dialog only exist in the DOM when the
// server rendered this page as authenticated (see pages.go's
// Authenticated gate on GET / -- ARCHITECTURE.md's home-dashboard-
// auth-bypass postmortem): an unauthenticated page load never ships that
// markup at all, so every lookup below can come back null. Login success
// navigates to "/" (see the submit handler) rather than revealing this
// markup in place, so by the time any of it is actually used the page has
// always been freshly, authenticated-ly rendered -- but the lookups and
// listener wiring still happen unconditionally at module load on *every*
// load, authenticated or not, so they're guarded against null here.
import { api, setCSRFToken } from "./api.js";
import { wireSettingsForm } from "./settingsForm.js";
import { initSidebar } from "./sidebar.js";
import { resolveIdentity, renderAvatar } from "./avatar.js";

const TICKS_PER_SECOND = 10_000_000;

const loginSection = document.getElementById("login-section");
const homeSection = document.getElementById("home-section");
const loginForm = document.getElementById("login-form");
const loginError = document.getElementById("login-error");
const welcomeHeading = document.getElementById("welcome-heading");

const createBtn = document.getElementById("create-party-btn");
const createDialog = document.getElementById("create-party-dialog");
const createForm = document.getElementById("create-party-form");
const createError = document.getElementById("create-error");
const cancelCreateBtn = document.getElementById("cancel-create-btn");
const createSettingsForm = homeSection
  ? wireSettingsForm({
      autoAdvance: "create-auto-advance", showNextDialog: "create-show-next-dialog",
      autoplayEnabled: "create-autoplay-enabled", autoplayDelay: "create-autoplay-delay",
    })
  : null;

const activeGrid = document.getElementById("active-parties-grid");
const activeCount = document.getElementById("active-count");
const yourSection = document.getElementById("your-parties-section");
const yourList = document.getElementById("your-parties-list");
const yourCount = document.getElementById("your-count");

const sidebar = homeSection ? initSidebar({ onLoggedOut: showLogin }) : null;

let me = null;

function showError(el, message) {
  el.textContent = message;
  el.hidden = false;
}

function hideError(el) {
  el.hidden = true;
}

function formatTicks(ticks) {
  const totalSeconds = Math.max(0, Math.round(ticks / TICKS_PER_SECOND));
  const h = Math.floor(totalSeconds / 3600);
  const m = Math.floor((totalSeconds % 3600) / 60);
  const s = totalSeconds % 60;
  const mm = String(m).padStart(h > 0 ? 2 : 1, "0");
  const ss = String(s).padStart(2, "0");
  return h > 0 ? `${h}:${String(m).padStart(2, "0")}:${ss}` : `${mm}:${ss}`;
}

async function init() {
  try {
    me = await api("/api/me");
    setCSRFToken(me.csrf_token);
    showHome();
  } catch {
    showLogin();
  }
}

function showLogin() {
  loginSection.hidden = false;
  // homeSection doesn't exist in the DOM on an unauthenticated page load
  // (see the module-header comment) -- this runs from init()'s /api/me
  // failure on exactly that load, so it must be guarded.
  if (homeSection) homeSection.hidden = true;
}

async function showHome() {
  loginSection.hidden = true;
  homeSection.hidden = false;
  sidebar.setUser(me);
  welcomeHeading.textContent = `Welcome back, ${me.user.display_name} 👋`;
  await loadParties();
}

function renderActiveCard(p) {
  const el = document.createElement("div");
  el.className = "party-card";
  const statusLabel = p.item_id ? (p.is_playing ? "Playing" : "Paused") : "Idle";
  el.innerHTML = `
    <div class="party-art">
      <span class="status-chip ${p.is_playing ? "playing" : "paused"}">${statusLabel}</span>
      <span class="watching-chip">
        <svg viewBox="0 0 24 24" fill="none"><circle cx="9" cy="8" r="2.6" stroke="currentColor" stroke-width="1.6"/><path d="M3.5 18c.4-2.8 2.5-4.5 5.5-4.5s5.1 1.7 5.5 4.5" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>
        ${p.member_count} watching
      </span>
      <div class="party-art-title">
        <h3></h3>
      </div>
    </div>
    <div class="party-card-body">
      <div class="host-row">
        <div class="avatar" style="width:24px;height:24px;font-size:10px;"></div>
        <span class="host-row-text"></span>
      </div>
      <div class="mini-scrub">
        <span class="mini-time">${formatTicks(p.position_ticks)}</span>
        <div class="mini-track"><div class="mini-fill ${p.is_playing ? "" : "is-paused"}" style="width:${p.duration_ticks > 0 ? Math.min(100, (p.position_ticks / p.duration_ticks) * 100) : 0}%;"></div></div>
        <span class="mini-time">${formatTicks(p.duration_ticks)}</span>
      </div>
      <button class="btn btn-primary join-btn">Join Party</button>
    </div>
  `;
  el.querySelector(".party-art-title h3").textContent = p.name;
  if (p.item_title) {
    el.querySelector(".party-art-title").insertAdjacentHTML("beforeend", `<span></span>`);
    el.querySelector(".party-art-title span").textContent = p.item_title;
  }
  const hostAvatarEl = el.querySelector(".host-row .avatar");
  // Resolved via the *viewer's* own Emby token (same as chat/Attendees),
  // never the host's -- see ARCHITECTURE.md's Party chat section. Renders
  // initials immediately, upgraded to a real picture once resolved.
  // Guarded against a detached card the same way player.js's renderMembers
  // guards its rows: loadParties() re-runs (rebuilding activeGrid from
  // scratch) on a logout/login cycle that doesn't reload the page, so a
  // slow lookup from a previous render can still be in flight once this
  // card is gone.
  renderAvatar(hostAvatarEl, { avatarUrl: "", displayName: p.host_display_name });
  resolveIdentity(p.host_user_id).then((identity) => {
    if (!hostAvatarEl.isConnected) return;
    renderAvatar(hostAvatarEl, { avatarUrl: identity.avatarUrl, displayName: p.host_display_name });
  });
  el.querySelector(".host-row-text").textContent = `Hosted by ${p.host_display_name}`;
  el.querySelector(".join-btn").addEventListener("click", () => {
    window.location.href = `/party/${encodeURIComponent(p.party_id)}`;
  });
  return el;
}

function renderYourRow(p) {
  const el = document.createElement("div");
  el.className = "your-party-row";
  el.innerHTML = `
    <div class="row-thumb"></div>
    <div class="row-text">
      <h4></h4>
      <div class="row-meta">Created by you · ${p.member_count} watching</div>
    </div>
    <div class="row-status">
      <span class="status-chip ${p.is_playing ? "playing" : "paused"}">${p.is_playing ? "Playing" : "Paused"}</span>
      <span class="mini-time">${formatTicks(p.position_ticks)} / ${formatTicks(p.duration_ticks)}</span>
    </div>
    <div class="row-actions">
      <button class="btn btn-ghost">Open Party</button>
    </div>
  `;
  el.querySelector(".row-text h4").textContent = p.name;
  el.querySelector(".row-actions button").addEventListener("click", () => {
    window.location.href = `/party/${encodeURIComponent(p.party_id)}`;
  });
  return el;
}

async function loadParties() {
  let parties;
  try {
    const result = await api("/api/parties");
    parties = result.parties || [];
  } catch {
    parties = [];
  }

  activeGrid.innerHTML = "";
  if (parties.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty-state";
    empty.textContent = "No parties are active right now — start one with Create Party.";
    activeGrid.appendChild(empty);
  } else {
    for (const p of parties) activeGrid.appendChild(renderActiveCard(p));
  }
  activeCount.textContent = String(parties.length);

  const yours = parties.filter((p) => p.host_user_id === me.user.id);
  yourSection.hidden = yours.length === 0;
  yourList.innerHTML = "";
  for (const p of yours) yourList.appendChild(renderYourRow(p));
  yourCount.textContent = String(yours.length);
}

loginForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  hideError(loginError);
  const formData = new FormData(loginForm);
  try {
    const result = await api("/api/auth/login", {
      method: "POST",
      body: { username: formData.get("username"), password: formData.get("password") },
    });
    // home-section isn't in this document -- an unauthenticated page load
    // never ships it (see the module-header comment above). Navigate
    // instead of showHome()-ing in place, so the now-authenticated
    // GET / renders the real dashboard shell for init() to reveal, the
    // same path any returning already-logged-in visitor already takes.
    setCSRFToken(result.csrf_token);
    window.location.href = "/";
  } catch (err) {
    showError(loginError, err.message);
  }
});

if (homeSection) {
  createBtn.addEventListener("click", () => {
    hideError(createError);
    createForm.reset();
    createSettingsForm.resync();
    createDialog.showModal();
  });
  cancelCreateBtn.addEventListener("click", () => createDialog.close());
  createForm.addEventListener("submit", async (e) => {
    e.preventDefault();
    hideError(createError);
    const formData = new FormData(createForm);
    try {
      const result = await api("/api/parties", {
        method: "POST",
        body: { name: formData.get("name"), ...createSettingsForm.read() },
      });
      window.location.href = `/party/${encodeURIComponent(result.party_id)}`;
    } catch (err) {
      showError(createError, err.message);
    }
  });
}

init();
