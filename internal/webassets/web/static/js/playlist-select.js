// Pure selection-state logic for the Add to Playlist browser's checkbox
// multi-select (movies, TV series/season/episode drill-down) -- no DOM, no
// fetch, so it can be unit tested directly (playlist-select.test.mjs) the
// same way internal/syncalg's math is mirrored and tested in sync.js,
// independent of the WebSocket/player wiring around it.
//
// A "selection entry" is one of:
//   {type: "movie", id, title, year}
//   {type: "episode", id, seriesName, seasonIndex, episodeIndex, title}
// playlist.js owns the actual selection Map (id -> entry) and the
// series/season fetch caches; everything here operates on plain values it
// is handed, so it never needs to know about the DOM or the network.

// Threshold at which checking a single series/season checkbox requires
// confirmation before committing (see the plan's "Confirmation dialog for
// large recursive selections") -- not applied to the final Add button.
export const LARGE_SELECTION_THRESHOLD = 25;

// Sort rule for the Selected Media panel, and therefore for the order items
// are sent to the batch-add endpoint: movies group first, then TV -- this
// mirrors the browse dialog's own tab order (Movies before TV Shows), not
// an arbitrary choice. Within movies: alphabetical by title
// (case-insensitive). Within TV: [seriesName, seasonIndex, episodeIndex]
// (seriesName case-insensitive). Ties within either group are broken by id
// for determinism. Movies and episodes never interleave by title.
export function sortSelection(entries) {
  const rank = (e) => (e.type === "movie" ? 0 : 1);
  return [...entries].sort((a, b) => {
    if (rank(a) !== rank(b)) return rank(a) - rank(b);
    if (a.type === "movie") {
      const t = a.title.localeCompare(b.title, undefined, { sensitivity: "base" });
      return t !== 0 ? t : a.id.localeCompare(b.id);
    }
    const s = a.seriesName.localeCompare(b.seriesName, undefined, { sensitivity: "base" });
    if (s !== 0) return s;
    if (a.seasonIndex !== b.seasonIndex) return a.seasonIndex - b.seasonIndex;
    if (a.episodeIndex !== b.episodeIndex) return a.episodeIndex - b.episodeIndex;
    return a.id.localeCompare(b.id);
  });
}

// Checkbox tri-state derivation for a series/season node: given the full
// set of that node's descendant leaf (episode) ids and the currently
// selected id set, returns which of the three states to render.
export function deriveCheckboxState(childIds, selectedIds) {
  if (childIds.length === 0) return "unchecked";
  let selectedCount = 0;
  for (const id of childIds) {
    if (selectedIds.has(id)) selectedCount++;
  }
  if (selectedCount === 0) return "unchecked";
  if (selectedCount === childIds.length) return "checked";
  return "indeterminate";
}

// How many of leafEntries are NOT yet in selectedIds -- what checking a
// series/season checkbox would newly add. Used to decide whether the
// LARGE_SELECTION_THRESHOLD confirmation applies.
export function countNewSelections(leafEntries, selectedIds) {
  let n = 0;
  for (const e of leafEntries) {
    if (!selectedIds.has(e.id)) n++;
  }
  return n;
}

// Returns a new Map with every entry in leafEntries added (or overwritten)
// -- used both for a single movie/episode toggle-on (a one-entry list) and
// for a series/season checkbox recursively selecting every episode beneath
// it. Never mutates selectionMap.
export function selectAll(selectionMap, leafEntries) {
  const next = new Map(selectionMap);
  for (const e of leafEntries) next.set(e.id, e);
  return next;
}

// Returns a new Map with every id in leafIds removed. Never mutates
// selectionMap.
export function deselectAll(selectionMap, leafIds) {
  const next = new Map(selectionMap);
  for (const id of leafIds) next.delete(id);
  return next;
}

export function formatSelectedLabel(entry) {
  return entry.type === "movie"
    ? `(Movie) ${entry.title}${entry.year ? `, ${entry.year}` : ""}`
    : `(TV) ${entry.seriesName} - S${entry.seasonIndex}E${entry.episodeIndex}`;
}

export function addButtonLabel(count) {
  return `Add ${count} Selected Item${count === 1 ? "" : "s"}`;
}

// Playlist reorder drag-and-drop math -- pure so the drop-position
// arithmetic can be unit tested independent of DOM/drag events, the same
// split as the selection logic above. Returns a new ordered id array with
// sourceId moved to just before (placeAfter false) or after (placeAfter
// true) targetId. No-ops (returns ids unchanged, not a copy) if
// sourceId === targetId or if targetId isn't present (a stale drop target,
// e.g. the playlist changed mid-drag).
export function reorderIds(ids, sourceId, targetId, placeAfter) {
  if (sourceId === targetId) return ids;
  const without = ids.filter((id) => id !== sourceId);
  const targetIndex = without.indexOf(targetId);
  if (targetIndex === -1) return ids;
  without.splice(targetIndex + (placeAfter ? 1 : 0), 0, sourceId);
  return without;
}
