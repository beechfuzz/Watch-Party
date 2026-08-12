// Index page: login form, then create/join party. Server is the source of
// truth; this just renders whatever /api/me and /api/parties/* say.
import { api, setCSRFToken, clearCSRFToken } from "./api.js";

const loginSection = document.getElementById("login-section");
const homeSection = document.getElementById("home-section");
const loginForm = document.getElementById("login-form");
const loginError = document.getElementById("login-error");
const whoami = document.getElementById("whoami");
const logoutBtn = document.getElementById("logout-btn");
const createForm = document.getElementById("create-party-form");
const createError = document.getElementById("create-error");
const joinForm = document.getElementById("join-party-form");
const joinError = document.getElementById("join-error");

async function init() {
  try {
    const me = await api("/api/me");
    setCSRFToken(me.csrf_token);
    showHome(me);
  } catch {
    showLogin();
  }
}

function showLogin() {
  loginSection.hidden = false;
  homeSection.hidden = true;
}

function showHome(me) {
  loginSection.hidden = true;
  homeSection.hidden = false;
  whoami.textContent = `Signed in as ${me.user.display_name}`;
}

loginForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  loginError.textContent = "";
  const formData = new FormData(loginForm);
  try {
    const result = await api("/api/auth/login", {
      method: "POST",
      body: { username: formData.get("username"), password: formData.get("password") },
    });
    setCSRFToken(result.csrf_token);
    showHome(result);
  } catch (err) {
    loginError.textContent = err.message;
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

createForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  createError.textContent = "";
  const formData = new FormData(createForm);
  try {
    const result = await api("/api/parties", { method: "POST", body: { item_id: formData.get("item_id") } });
    window.location.href = `/party/${encodeURIComponent(result.party_id)}`;
  } catch (err) {
    createError.textContent = err.message;
  }
});

joinForm.addEventListener("submit", (e) => {
  e.preventDefault();
  joinError.textContent = "";
  const formData = new FormData(joinForm);
  const partyId = (formData.get("party_id") || "").trim();
  if (!partyId) {
    joinError.textContent = "Enter a party ID or link.";
    return;
  }
  // Accept either a bare ID or a pasted /party/<id> URL.
  const match = partyId.match(/\/party\/([^/?#]+)/);
  window.location.href = `/party/${encodeURIComponent(match ? match[1] : partyId)}`;
});

init();
