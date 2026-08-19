// Party page: the Playlist section (list + host-only play/remove controls)
// and its "Add to playlist" browse dialog (search box, Movies/TV
// Shows/Recently Added/Favorites tabs, poster grid with series→season→
// episode drill-down and checkbox multi-select). Library browsing goes
// through this app's own backend (GET /api/emby/items and friends), not
// straight to Emby from the browser — unlike the playback/poster URLs,
// which are deliberately direct-to-Emby. See ARCHITECTURE.md's Playlist
// section for why the two cases are treated differently.
//
// Selection bookkeeping (the Map, sort order, checkbox tri-state, label
// formatting) lives in playlist-select.js as pure functions with no DOM/
// fetch dependency, so it can be unit tested directly — this module is
// just the DOM/network wiring around it.
import { api } from "./api.js";
import {
  LARGE_SELECTION_THRESHOLD,
  sortSelection,
  deriveCheckboxState,
  countNewSelections,
  selectAll,
  deselectAll,
  formatSelectedLabel,
  addButtonLabel,
} from "./playlist-select.js";

const playlistItemsEl = document.getElementById("playlist-items");
const addBtn = document.getElementById("add-to-playlist-btn");
const browseDialog = document.getElementById("browse-dialog");
const browseCloseBtn = document.getElementById("browse-close-btn");
const browseSearch = document.getElementById("browse-search");
const browseTabs = document.getElementById("browse-tabs");
const browseBreadcrumb = document.getElementById("browse-breadcrumb");
const browseGrid = document.getElementById("browse-grid");
const browseError = document.getElementById("browse-error");
const selectedMediaList = document.getElementById("selected-media-list");
const selectedMediaAddBtn = document.getElementById("selected-media-add-btn");
const confirmLargeDialog = document.getElementById("confirm-large-selection-dialog");
const confirmLargeText = document.getElementById("confirm-large-selection-text");
const confirmLargeCancelBtn = document.getElementById("confirm-large-selection-cancel-btn");
const confirmLargeAddBtn = document.getElementById("confirm-large-selection-add-btn");

let partyId = null;
let isHostFn = () => false;
let currentTab = "movies";
let searchDebounceTimer = null;

// Drill-down navigation stack: [] = tab/search root. An entry is
// {type: "series", id, name} or {type: "season", id, name, seriesId, seriesName}.
let browseStack = [];

// In-memory caches, populated by drill-down navigation AND by recursive
// checkbox selection (whichever happens first) — checking a series/season
// checkbox without ever navigating into it still needs this data, so it's
// fetched on demand either way.
let seasonsCache = new Map(); // seriesId -> SeasonSummary[]
let episodesCache = new Map(); // seasonId -> EpisodeSummary[]

// Selection state: Emby ItemId -> selection entry (see playlist-select.js's
// module doc comment for the entry shape). Single source of truth the
// Selected Media panel renders from.
let selection = new Map();

// Checkbox nodes currently rendered in the grid, so a selection-map mutation
// can refresh their tri-state without a full re-render (re-fetching would be
// wasteful, and would also flash the "no results yet" state).
let visibleNodes = []; // {kind: "movie" | "episode" | "series" | "season", id, el}

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

// --- browse dialog: drill-down + checkbox multi-select ---

function setCheckboxState(el, state) {
  el.checked = state === "checked";
  el.indeterminate = state === "indeterminate";
}

function trackNode(node) {
  visibleNodes.push(node);
}

// The full leaf (episode) id list for a series/season, from cache only —
// null means "not fetched yet", which is always the same as "nothing under
// it is selected" (an episode can only ever enter `selection` after its
// season has been fetched), so callers treat null as unchecked rather than
// triggering a fetch just to render a checkbox.
function cachedLeafIdsForSeries(seriesId) {
  const seasons = seasonsCache.get(seriesId);
  if (!seasons) return null;
  let ids = [];
  for (const s of seasons) {
    const eps = episodesCache.get(s.id);
    if (!eps) return null;
    ids = ids.concat(eps.map((e) => e.id));
  }
  return ids;
}

function cachedLeafIdsForSeason(seasonId) {
  const eps = episodesCache.get(seasonId);
  return eps ? eps.map((e) => e.id) : null;
}

function refreshVisibleCheckboxStates() {
  const selectedIds = new Set(selection.keys());
  for (const n of visibleNodes) {
    if (n.kind === "movie" || n.kind === "episode") {
      setCheckboxState(n.el, selectedIds.has(n.id) ? "checked" : "unchecked");
    } else if (n.kind === "series") {
      const leafIds = cachedLeafIdsForSeries(n.id);
      setCheckboxState(n.el, leafIds ? deriveCheckboxState(leafIds, selectedIds) : "unchecked");
    } else if (n.kind === "season") {
      const leafIds = cachedLeafIdsForSeason(n.id);
      setCheckboxState(n.el, leafIds ? deriveCheckboxState(leafIds, selectedIds) : "unchecked");
    }
  }
}

async function fetchSeasons(seriesId) {
  if (seasonsCache.has(seriesId)) return seasonsCache.get(seriesId);
  const result = await api(`/api/emby/series/${encodeURIComponent(seriesId)}/seasons`);
  const seasons = result.seasons || [];
  seasonsCache.set(seriesId, seasons);
  return seasons;
}

async function fetchEpisodes(seriesId, seasonId) {
  if (episodesCache.has(seasonId)) return episodesCache.get(seasonId);
  const result = await api(`/api/emby/seasons/${encodeURIComponent(seasonId)}/episodes?series_id=${encodeURIComponent(seriesId)}`);
  const episodes = result.episodes || [];
  episodesCache.set(seasonId, episodes);
  return episodes;
}

function episodeToEntry(ep) {
  return {
    type: "episode",
    id: ep.id,
    seriesName: ep.series_name,
    seasonIndex: ep.season_index_number,
    episodeIndex: ep.index_number,
    title: ep.name,
  };
}

async function fetchAllEpisodesForSeries(seriesId) {
  const seasons = await fetchSeasons(seriesId);
  const lists = await Promise.all(seasons.map((s) => fetchEpisodes(seriesId, s.id)));
  return lists.flat().map(episodeToEntry);
}

async function fetchAllEpisodesForSeason(seriesId, seasonId) {
  const episodes = await fetchEpisodes(seriesId, seasonId);
  return episodes.map(episodeToEntry);
}

// confirmLargeSelection shows the styled confirmation dialog for a
// selection action that would newly add >= LARGE_SELECTION_THRESHOLD items
// — a real <dialog> matching the rest of this app's modal styling, not the
// browser's native confirm(). Resolves true/false with the user's choice.
function confirmLargeSelection(count) {
  return new Promise((resolve) => {
    confirmLargeText.textContent = `Add ${count} items to the playlist?`;
    confirmLargeDialog.showModal();
    const cleanup = () => {
      confirmLargeAddBtn.removeEventListener("click", onAdd);
      confirmLargeCancelBtn.removeEventListener("click", onCancel);
      confirmLargeDialog.removeEventListener("cancel", onCancel);
    };
    const onAdd = () => {
      cleanup();
      confirmLargeDialog.close();
      resolve(true);
    };
    const onCancel = () => {
      cleanup();
      confirmLargeDialog.close();
      resolve(false);
    };
    confirmLargeAddBtn.addEventListener("click", onAdd);
    confirmLargeCancelBtn.addEventListener("click", onCancel);
    confirmLargeDialog.addEventListener("cancel", onCancel);
  });
}

function onMovieCheckboxToggle(item, checked) {
  const entry = { type: "movie", id: item.id, title: item.name, year: item.year };
  selection = checked ? selectAll(selection, [entry]) : deselectAll(selection, [entry.id]);
  renderSelectedMediaPanel();
  refreshVisibleCheckboxStates();
}

function onEpisodeCheckboxToggle(ep, checked) {
  const entry = episodeToEntry(ep);
  selection = checked ? selectAll(selection, [entry]) : deselectAll(selection, [entry.id]);
  renderSelectedMediaPanel();
  refreshVisibleCheckboxStates();
}

// Series/season checkbox: fetches (and caches) that node's full episode
// list if it isn't already cached — even if the user never drills into it —
// showing a loading state on the checkbox while that resolves. Checking
// applies the >= LARGE_SELECTION_THRESHOLD confirmation; unchecking never
// does (removal isn't the risk that guards against).
async function onSeriesOrSeasonCheckboxToggle(checked, checkboxEl, fetchLeaves) {
  checkboxEl.disabled = true;
  checkboxEl.closest(".browse-card")?.classList.add("is-loading");
  try {
    const leaves = await fetchLeaves();
    if (checked) {
      const selectedIds = new Set(selection.keys());
      const newCount = countNewSelections(leaves, selectedIds);
      if (newCount >= LARGE_SELECTION_THRESHOLD) {
        const confirmed = await confirmLargeSelection(newCount);
        if (!confirmed) {
          setCheckboxState(checkboxEl, "unchecked");
          return;
        }
      }
      selection = selectAll(selection, leaves);
    } else {
      selection = deselectAll(selection, leaves.map((e) => e.id));
    }
    renderSelectedMediaPanel();
  } catch (err) {
    browseError.textContent = err.message;
    browseError.hidden = false;
  } finally {
    checkboxEl.disabled = false;
    checkboxEl.closest(".browse-card")?.classList.remove("is-loading");
    refreshVisibleCheckboxStates();
  }
}

// --- card rendering ---

function createCard({ title, posterURL, onNavigate, checkboxState, checkboxLabel, onCheckboxChange }) {
  const card = document.createElement("div");
  const navigable = typeof onNavigate === "function";
  card.className = "browse-card" + (navigable ? " is-navigable" : "");
  card.innerHTML = `<div class="browse-card-thumb"></div><div class="browse-card-title"></div>`;
  if (posterURL) {
    card.querySelector(".browse-card-thumb").style.backgroundImage = `url("${posterURL}")`;
  }
  card.querySelector(".browse-card-title").textContent = title;

  const checkbox = document.createElement("input");
  checkbox.type = "checkbox";
  checkbox.className = "browse-card-checkbox";
  checkbox.setAttribute("aria-label", checkboxLabel || `Select ${title}`);
  setCheckboxState(checkbox, checkboxState);
  // stopPropagation on the checkbox specifically -- independent of the
  // card's own click-to-navigate handler below -- so a checkbox click never
  // triggers drill-down navigation.
  checkbox.addEventListener("click", (e) => e.stopPropagation());
  checkbox.addEventListener("change", (e) => {
    e.stopPropagation();
    onCheckboxChange(checkbox.checked, checkbox);
  });
  card.appendChild(checkbox);

  if (navigable) {
    card.tabIndex = 0;
    card.setAttribute("role", "button");
    card.addEventListener("click", onNavigate);
    card.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        onNavigate();
      }
    });
  }

  return { el: card, checkbox };
}

function renderRootCard(item) {
  const isSeries = item.type === "Series";
  const checkboxState = isSeries
    ? (() => {
        const leafIds = cachedLeafIdsForSeries(item.id);
        return leafIds ? deriveCheckboxState(leafIds, new Set(selection.keys())) : "unchecked";
      })()
    : selection.has(item.id)
      ? "checked"
      : "unchecked";
  const { el, checkbox } = createCard({
    title: item.year ? `${item.name} (${item.year})` : item.name,
    posterURL: item.poster_url,
    onNavigate: isSeries
      ? () => {
          browseStack.push({ type: "series", id: item.id, name: item.name });
          renderCurrentLevel();
        }
      : undefined,
    checkboxState,
    onCheckboxChange: isSeries
      ? (checked, checkboxEl) => onSeriesOrSeasonCheckboxToggle(checked, checkboxEl, () => fetchAllEpisodesForSeries(item.id))
      : (checked) => onMovieCheckboxToggle(item, checked),
  });
  trackNode({ kind: isSeries ? "series" : "movie", id: item.id, el: checkbox });
  return el;
}

function renderSeasonCard(season) {
  const label = season.name || `Season ${season.index_number}`;
  const leafIds = cachedLeafIdsForSeason(season.id);
  const checkboxState = leafIds ? deriveCheckboxState(leafIds, new Set(selection.keys())) : "unchecked";
  const { el, checkbox } = createCard({
    title: label,
    posterURL: season.poster_url,
    onNavigate: () => {
      browseStack.push({
        type: "season", id: season.id, name: label,
        seriesId: season.series_id, seriesName: season.series_name,
      });
      renderCurrentLevel();
    },
    checkboxState,
    onCheckboxChange: (checked, checkboxEl) =>
      onSeriesOrSeasonCheckboxToggle(checked, checkboxEl, () => fetchAllEpisodesForSeason(season.series_id, season.id)),
  });
  trackNode({ kind: "season", id: season.id, el: checkbox });
  return el;
}

function renderEpisodeCard(ep) {
  const { el, checkbox } = createCard({
    title: `${ep.index_number}. ${ep.name}`,
    posterURL: ep.poster_url,
    checkboxState: selection.has(ep.id) ? "checked" : "unchecked",
    onCheckboxChange: (checked) => onEpisodeCheckboxToggle(ep, checked),
  });
  trackNode({ kind: "episode", id: ep.id, el: checkbox });
  return el;
}

function renderEmpty(text) {
  const empty = document.createElement("div");
  empty.className = "playlist-empty";
  empty.textContent = text;
  browseGrid.appendChild(empty);
}

function tabLabel(tab) {
  return { movies: "Movies", tv: "TV Shows", recent: "Recently Added", favorites: "Favorites" }[tab] || "Browse";
}

function renderBreadcrumb() {
  browseBreadcrumb.innerHTML = "";
  if (browseStack.length === 0) {
    browseBreadcrumb.hidden = true;
    return;
  }
  browseBreadcrumb.hidden = false;
  const rootLabel = browseSearch.value.trim() ? "Search results" : tabLabel(currentTab);
  const segments = [{ label: rootLabel, index: -1 }, ...browseStack.map((s, i) => ({ label: s.name, index: i }))];
  segments.forEach((seg, i) => {
    if (i > 0) {
      const sep = document.createElement("span");
      sep.className = "browse-breadcrumb-sep";
      sep.textContent = "›";
      browseBreadcrumb.appendChild(sep);
    }
    const isLast = i === segments.length - 1;
    const el = document.createElement(isLast ? "span" : "button");
    if (!isLast) el.type = "button";
    el.className = "browse-breadcrumb-item" + (isLast ? " is-current" : "");
    el.textContent = seg.label;
    if (!isLast) {
      el.addEventListener("click", () => {
        browseStack = browseStack.slice(0, seg.index + 1);
        renderCurrentLevel();
      });
    }
    browseBreadcrumb.appendChild(el);
  });
}

async function renderRootLevel() {
  const params = new URLSearchParams();
  const term = browseSearch.value.trim();
  if (term) {
    params.set("search", term);
  } else {
    params.set("tab", currentTab);
  }
  try {
    const result = await api(`/api/emby/items?${params.toString()}`);
    const items = result.items || [];
    if (items.length === 0) {
      renderEmpty("No results.");
    } else {
      for (const item of items) browseGrid.appendChild(renderRootCard(item));
    }
  } catch (err) {
    browseError.textContent = err.message;
    browseError.hidden = false;
  }
}

async function renderSeasonsLevel(top) {
  try {
    const seasons = await fetchSeasons(top.id);
    if (seasons.length === 0) {
      renderEmpty("No seasons found.");
    } else {
      for (const season of seasons) browseGrid.appendChild(renderSeasonCard(season));
    }
  } catch (err) {
    browseError.textContent = err.message;
    browseError.hidden = false;
  }
}

async function renderEpisodesLevel(top) {
  try {
    const episodes = await fetchEpisodes(top.seriesId, top.id);
    if (episodes.length === 0) {
      renderEmpty("No episodes found.");
    } else {
      for (const ep of episodes) browseGrid.appendChild(renderEpisodeCard(ep));
    }
  } catch (err) {
    browseError.textContent = err.message;
    browseError.hidden = false;
  }
}

async function renderCurrentLevel() {
  browseError.hidden = true;
  browseGrid.innerHTML = "";
  visibleNodes = [];
  renderBreadcrumb();
  if (browseStack.length === 0) {
    await renderRootLevel();
  } else {
    const top = browseStack[browseStack.length - 1];
    if (top.type === "series") {
      await renderSeasonsLevel(top);
    } else {
      await renderEpisodesLevel(top);
    }
  }
  // A fresh render can't know which nodes are indeterminate until its own
  // fetch resolves cache entries -- re-run once against the now-current
  // cache/selection so series/season checkboxes reflect real state rather
  // than the "unchecked" placeholder they were created with.
  refreshVisibleCheckboxStates();
}

// --- Selected Media panel ---

function renderSelectedMediaRow(entry) {
  const row = document.createElement("div");
  row.className = "selected-media-item";
  const label = document.createElement("span");
  label.className = "selected-media-label";
  label.textContent = formatSelectedLabel(entry);
  row.appendChild(label);

  const removeBtn = document.createElement("button");
  removeBtn.type = "button";
  removeBtn.className = "selected-media-remove";
  removeBtn.title = "Remove";
  removeBtn.textContent = "−";
  removeBtn.addEventListener("click", () => {
    selection = deselectAll(selection, [entry.id]);
    renderSelectedMediaPanel();
    refreshVisibleCheckboxStates();
  });
  row.appendChild(removeBtn);
  return row;
}

function renderSelectedMediaPanel() {
  const sorted = sortSelection([...selection.values()]);
  selectedMediaList.innerHTML = "";
  if (sorted.length === 0) {
    const empty = document.createElement("div");
    empty.className = "playlist-empty";
    empty.textContent = "Nothing selected yet.";
    selectedMediaList.appendChild(empty);
  } else {
    for (const entry of sorted) selectedMediaList.appendChild(renderSelectedMediaRow(entry));
  }
  selectedMediaAddBtn.textContent = addButtonLabel(sorted.length);
  selectedMediaAddBtn.disabled = sorted.length === 0;
}

selectedMediaAddBtn.addEventListener("click", async () => {
  const sorted = sortSelection([...selection.values()]);
  if (sorted.length === 0) return;
  selectedMediaAddBtn.disabled = true;
  try {
    await api(`/api/parties/${encodeURIComponent(partyId)}/playlist/batch`, {
      method: "POST",
      body: { item_ids: sorted.map((e) => e.id) },
    });
    browseDialog.close();
    await loadPlaylist();
  } catch (err) {
    browseError.textContent = err.message;
    browseError.hidden = false;
    selectedMediaAddBtn.disabled = selection.size === 0;
  }
});

// --- dialog open/close, tabs, search ---

function resetBrowseState() {
  selection = new Map();
  browseStack = [];
  seasonsCache = new Map();
  episodesCache = new Map();
  visibleNodes = [];
  renderSelectedMediaPanel();
}

addBtn.addEventListener("click", () => {
  browseSearch.value = "";
  browseError.hidden = true;
  resetBrowseState();
  browseDialog.showModal();
  renderCurrentLevel();
});
browseCloseBtn.addEventListener("click", () => browseDialog.close());
browseDialog.addEventListener("close", () => resetBrowseState());

browseTabs.addEventListener("click", (e) => {
  const btn = e.target.closest(".browse-tab");
  if (!btn) return;
  for (const t of browseTabs.querySelectorAll(".browse-tab")) t.classList.remove("is-active");
  btn.classList.add("is-active");
  currentTab = btn.dataset.tab;
  browseSearch.value = "";
  browseStack = [];
  renderCurrentLevel();
});
browseSearch.addEventListener("input", () => {
  clearTimeout(searchDebounceTimer);
  searchDebounceTimer = setTimeout(() => {
    browseStack = [];
    renderCurrentLevel();
  }, 300);
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
