// Index page: login form, then the dashboard (active/your parties +
// create). Server is the source of truth; this just renders whatever
// /api/me and /api/parties say.
import { api, setCSRFToken, clearCSRFToken } from "./api.js";

const TICKS_PER_SECOND = 10_000_000;

const loginSection = document.getElementById("login-section");
const homeSection = document.getElementById("home-section");
const loginForm = document.getElementById("login-form");
const loginError = document.getElementById("login-error");
const userAvatarInitial = document.getElementById("user-avatar-initial");
const userName = document.getElementById("user-name");
const welcomeHeading = document.getElementById("welcome-heading");
const logoutBtn = document.getElementById("logout-btn");

const createBtn = document.getElementById("create-party-btn");
const createDialog = document.getElementById("create-party-dialog");
const createForm = document.getElementById("create-party-form");
const createError = document.getElementById("create-error");
const cancelCreateBtn = document.getElementById("cancel-create-btn");

const activeGrid = document.getElementById("active-parties-grid");
const activeCount = document.getElementById("active-count");
const yourSection = document.getElementById("your-parties-section");
const yourList = document.getElementById("your-parties-list");
const yourCount = document.getElementById("your-count");

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

function initials(name) {
  return (name || "?").trim().slice(0, 1).toUpperCase();
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
  homeSection.hidden = true;
}

async function showHome() {
  loginSection.hidden = true;
  homeSection.hidden = false;
  userAvatarInitial.textContent = initials(me.user.display_name);
  userName.textContent = me.user.display_name;
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
  el.querySelector(".host-row .avatar").textContent = initials(p.host_display_name);
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
    me = result;
    setCSRFToken(result.csrf_token);
    showHome();
  } catch (err) {
    showError(loginError, err.message);
  }
});

logoutBtn.addEventListener("click", async () => {
  try {
    await api("/api/auth/logout", { method: "POST" });
  } finally {
    clearCSRFToken();
    showLogin();
  }
});

createBtn.addEventListener("click", () => {
  hideError(createError);
  createForm.reset();
  createDialog.showModal();
});
cancelCreateBtn.addEventListener("click", () => createDialog.close());
createForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  hideError(createError);
  const formData = new FormData(createForm);
  try {
    const result = await api("/api/parties", { method: "POST", body: { name: formData.get("name") } });
    window.location.href = `/party/${encodeURIComponent(result.party_id)}`;
  } catch (err) {
    showError(createError, err.message);
  }
});

init();
