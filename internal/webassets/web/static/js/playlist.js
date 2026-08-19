// Party page: the Playlist section (list + host-only play/remove controls)
// and its "Add to playlist" browse dialog (search box, Movies/TV
// Shows/Recently Added/Favorites tabs, poster grid). Library browsing goes
// through this app's own backend (GET /api/emby/items), not straight to
// Emby from the browser — unlike the playback/poster URLs, which are
// deliberately direct-to-Emby. See ARCHITECTURE.md's Playlist section for
// why the two cases are treated differently.
import { api } from "./api.js";

const playlistItemsEl = document.getElementById("playlist-items");
const addBtn = document.getElementById("add-to-playlist-btn");
const browseDialog = document.getElementById("browse-dialog");
const browseCloseBtn = document.getElementById("browse-close-btn");
const browseSearch = document.getElementById("browse-search");
const browseTabs = document.getElementById("browse-tabs");
const browseGrid = document.getElementById("browse-grid");
const browseError = document.getElementById("browse-error");

let partyId = null;
let isHostFn = () => false;
let currentTab = "movies";
let searchDebounceTimer = null;

function ticksToLabel(ticks) {
  if (!ticks) return "";
  const totalMinutes = Math.round(ticks / 10_000_000 / 60);
  const h = Math.floor(totalMinutes / 60);
  const m = totalMinutes % 60;
  return h > 0 ? `${h}h ${m}m` : `${m}m`;
}

function iconBtn(title, pathD) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = "playlist-item-btn";
  btn.title = title;
  btn.innerHTML = `<svg viewBox="0 0 24 24" fill="none">${pathD}</svg>`;
  return btn;
}

function renderPlaylistItem(item) {
  const row = document.createElement("div");
  row.className = `playlist-item${item.is_current ? " is-current" : ""}`;
  row.innerHTML = `
    <div class="playlist-item-thumb"></div>
    <div class="playlist-item-body">
      <div class="playlist-item-title"></div>
      <div class="playlist-item-meta"></div>
    </div>
  `;
  if (item.poster_url) {
    row.querySelector(".playlist-item-thumb").style.backgroundImage = `url("${item.poster_url}")`;
  }
  row.querySelector(".playlist-item-title").textContent = item.title;
  row.querySelector(".playlist-item-title").classList.toggle("is-restricted", !!item.restricted);
  row.querySelector(".playlist-item-meta").textContent = item.is_current
    ? "Now playing"
    : ticksToLabel(item.duration_ticks);

  if (isHostFn() && !item.is_current) {
    const playBtn = iconBtn("Play now", `<path d="M7 5l12 7-12 7V5z" fill="currentColor"/>`);
    playBtn.addEventListener("click", async () => {
      try {
        await api(`/api/parties/${encodeURIComponent(partyId)}/playlist/${item.id}/play`, { method: "POST" });
      } catch (err) {
        alert(err.message);
      }
    });
    row.appendChild(playBtn);

    const removeBtn = iconBtn("Remove", `<path d="M6 6l12 12M18 6L6 18" stroke="currentColor" stroke-width="1.7" stroke-linecap="round"/>`);
    removeBtn.addEventListener("click", async () => {
      try {
        await api(`/api/parties/${encodeURIComponent(partyId)}/playlist/${item.id}`, { method: "DELETE" });
        await loadPlaylist();
      } catch (err) {
        alert(err.message);
      }
    });
    row.appendChild(removeBtn);
  }
  return row;
}

export async function loadPlaylist() {
  let items = [];
  try {
    const result = await api(`/api/parties/${encodeURIComponent(partyId)}/playlist`);
    items = result.items || [];
  } catch {
    return; // best-effort; leave whatever was last rendered
  }
  playlistItemsEl.innerHTML = "";
  if (items.length === 0) {
    const empty = document.createElement("div");
    empty.className = "playlist-empty";
    empty.textContent = "Nothing queued yet.";
    playlistItemsEl.appendChild(empty);
  } else {
    for (const item of items) playlistItemsEl.appendChild(renderPlaylistItem(item));
  }
  addBtn.hidden = !isHostFn();
}

function renderBrowseCard(item) {
  const card = document.createElement("button");
  card.type = "button";
  card.className = "browse-card";
  card.innerHTML = `<div class="browse-card-thumb"></div><div class="browse-card-title"></div>`;
  if (item.poster_url) {
    card.querySelector(".browse-card-thumb").style.backgroundImage = `url("${item.poster_url}")`;
  }
  card.querySelector(".browse-card-title").textContent = item.year ? `${item.name} (${item.year})` : item.name;
  card.addEventListener("click", async () => {
    card.disabled = true;
    try {
      await api(`/api/parties/${encodeURIComponent(partyId)}/playlist`, { method: "POST", body: { item_id: item.id } });
      browseDialog.close();
      await loadPlaylist();
    } catch (err) {
      browseError.textContent = err.message;
      browseError.hidden = false;
      card.disabled = false;
    }
  });
  return card;
}

async function runBrowse() {
  browseError.hidden = true;
  const params = new URLSearchParams();
  const term = browseSearch.value.trim();
  if (term) {
    params.set("search", term);
  } else {
    params.set("tab", currentTab);
  }
  browseGrid.innerHTML = "";
  try {
    const result = await api(`/api/emby/items?${params.toString()}`);
    const items = result.items || [];
    if (items.length === 0) {
      const empty = document.createElement("div");
      empty.className = "playlist-empty";
      empty.textContent = "No results.";
      browseGrid.appendChild(empty);
    } else {
      for (const item of items) browseGrid.appendChild(renderBrowseCard(item));
    }
  } catch (err) {
    browseError.textContent = err.message;
    browseError.hidden = false;
  }
}

addBtn.addEventListener("click", () => {
  browseSearch.value = "";
  browseError.hidden = true;
  browseDialog.showModal();
  runBrowse();
});
browseCloseBtn.addEventListener("click", () => browseDialog.close());
browseTabs.addEventListener("click", (e) => {
  const btn = e.target.closest(".browse-tab");
  if (!btn) return;
  for (const t of browseTabs.querySelectorAll(".browse-tab")) t.classList.remove("is-active");
  btn.classList.add("is-active");
  currentTab = btn.dataset.tab;
  browseSearch.value = "";
  runBrowse();
});
browseSearch.addEventListener("input", () => {
  clearTimeout(searchDebounceTimer);
  searchDebounceTimer = setTimeout(runBrowse, 300);
});

/**
 * @param {string} id party id
 * @param {() => boolean} isHostCallback live reference to the party page's
 *   own isHost() check — called fresh on every render, not snapshotted once,
 *   so playlist controls follow host transfer correctly.
 */
export function initPlaylist(id, isHostCallback) {
  partyId = id;
  isHostFn = isHostCallback;
}
