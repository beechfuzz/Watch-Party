// Run with: node --test internal/webassets/web/static/js/playlist-select.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import {
  LARGE_SELECTION_THRESHOLD,
  sortSelection,
  deriveCheckboxState,
  countNewSelections,
  selectAll,
  deselectAll,
  formatSelectedLabel,
  addButtonLabel,
  reorderIds,
  formatEpisodeLine,
  positionRelativeToCurrent,
} from "./playlist-select.js";

const movie = (id, title, year) => ({ type: "movie", id, title, year });
const episode = (id, seriesName, seasonIndex, episodeIndex, title) => ({
  type: "episode",
  id,
  seriesName,
  seasonIndex,
  episodeIndex,
  title,
});

test("sortSelection: movies group first (alphabetical), then TV grouped by series/season/episode", () => {
  const entries = [
    episode("e2", "Andor", 1, 2, "Episode Two"),
    movie("m2", "Zodiac", 2007),
    episode("e1", "Andor", 1, 1, "Episode One"),
    movie("m1", "Alien", 1979),
    episode("e3", "Breaking Bad", 1, 1, "Pilot"),
  ];
  const sorted = sortSelection(entries).map((e) => e.id);
  // Movies first, alphabetical: Alien before Zodiac. Then TV grouped by
  // series (Andor before Breaking Bad), then season/episode order within
  // Andor -- movies and episodes never interleave by title.
  assert.deepEqual(sorted, ["m1", "m2", "e1", "e2", "e3"]);
});

test("sortSelection: within TV, orders by season then episode index, not id or fetch order", () => {
  const entries = [
    episode("e1", "Andor", 2, 1, "S2E1"),
    episode("e2", "Andor", 1, 5, "S1E5"),
    episode("e3", "Andor", 1, 1, "S1E1"),
  ];
  const sorted = sortSelection(entries).map((e) => e.id);
  assert.deepEqual(sorted, ["e3", "e2", "e1"]);
});

test("sortSelection: ties broken by id for determinism", () => {
  const entries = [movie("m2", "Same Title"), movie("m1", "Same Title")];
  const sorted = sortSelection(entries).map((e) => e.id);
  assert.deepEqual(sorted, ["m1", "m2"]);
});

test("sortSelection: does not mutate its input", () => {
  const entries = [movie("m2", "B"), movie("m1", "A")];
  const copy = [...entries];
  sortSelection(entries);
  assert.deepEqual(entries, copy);
});

test("deriveCheckboxState: none selected => unchecked", () => {
  assert.equal(deriveCheckboxState(["e1", "e2"], new Set()), "unchecked");
});

test("deriveCheckboxState: all selected => checked", () => {
  assert.equal(deriveCheckboxState(["e1", "e2"], new Set(["e1", "e2"])), "checked");
});

test("deriveCheckboxState: some selected => indeterminate", () => {
  assert.equal(deriveCheckboxState(["e1", "e2", "e3"], new Set(["e1"])), "indeterminate");
});

test("deriveCheckboxState: no children => unchecked (not indeterminate)", () => {
  assert.equal(deriveCheckboxState([], new Set(["e1"])), "unchecked");
});

test("countNewSelections: counts only ids not already selected", () => {
  const leaves = [episode("e1", "Andor", 1, 1, "One"), episode("e2", "Andor", 1, 2, "Two"), episode("e3", "Andor", 1, 3, "Three")];
  assert.equal(countNewSelections(leaves, new Set(["e2"])), 2);
  assert.equal(countNewSelections(leaves, new Set(["e1", "e2", "e3"])), 0);
});

test("LARGE_SELECTION_THRESHOLD is exactly 25", () => {
  assert.equal(LARGE_SELECTION_THRESHOLD, 25);
});

test("selectAll: recursively adds every leaf against a mock series/season/episode tree, without mutating the original map", () => {
  const mockSeries = {
    seasons: [
      { episodes: [episode("e1", "Andor", 1, 1, "One"), episode("e2", "Andor", 1, 2, "Two")] },
      { episodes: [episode("e3", "Andor", 2, 1, "S2E1")] },
    ],
  };
  const allLeaves = mockSeries.seasons.flatMap((s) => s.episodes);

  const original = new Map([["existing", movie("existing", "Pre-selected")]]);
  const next = selectAll(original, allLeaves);

  assert.equal(original.size, 1, "original map must not be mutated");
  assert.equal(next.size, 4);
  assert.ok(next.has("e1") && next.has("e2") && next.has("e3") && next.has("existing"));
});

test("deselectAll: recursively removes every leaf id, without mutating the original map", () => {
  const original = new Map([
    ["e1", episode("e1", "Andor", 1, 1, "One")],
    ["e2", episode("e2", "Andor", 1, 2, "Two")],
    ["keep", movie("keep", "Keep Me")],
  ]);
  const next = deselectAll(original, ["e1", "e2"]);

  assert.equal(original.size, 3, "original map must not be mutated");
  assert.equal(next.size, 1);
  assert.ok(next.has("keep"));
});

test("formatSelectedLabel: movie", () => {
  assert.equal(formatSelectedLabel(movie("m1", "Alien", 1979)), "(Movie) Alien, 1979");
});

test("formatSelectedLabel: movie without a year", () => {
  assert.equal(formatSelectedLabel(movie("m1", "Alien")), "(Movie) Alien");
});

test("formatSelectedLabel: episode", () => {
  assert.equal(formatSelectedLabel(episode("e1", "Andor", 1, 3, "Reckoning")), "(TV) Andor - S1E3");
});

test("addButtonLabel: singular vs plural", () => {
  assert.equal(addButtonLabel(1), "Add 1 Selected Item");
  assert.equal(addButtonLabel(2), "Add 2 Selected Items");
  assert.equal(addButtonLabel(0), "Add 0 Selected Items");
});

test("reorderIds: move item forward, placing it after the target", () => {
  const result = reorderIds([1, 2, 3, 4], 1, 3, true);
  assert.deepEqual(result, [2, 3, 1, 4]);
});

test("reorderIds: move item forward, placing it before the target", () => {
  const result = reorderIds([1, 2, 3, 4], 1, 3, false);
  assert.deepEqual(result, [2, 1, 3, 4]);
});

test("reorderIds: move item backward, placing it after the target", () => {
  const result = reorderIds([1, 2, 3, 4], 4, 1, true);
  assert.deepEqual(result, [1, 4, 2, 3]);
});

test("reorderIds: move item backward, placing it before the target", () => {
  const result = reorderIds([1, 2, 3, 4], 4, 1, false);
  assert.deepEqual(result, [4, 1, 2, 3]);
});

test("reorderIds: dropping on itself is a no-op", () => {
  const ids = [1, 2, 3];
  assert.deepEqual(reorderIds(ids, 2, 2, true), ids);
});

test("reorderIds: stale/unknown target id is a no-op", () => {
  const ids = [1, 2, 3];
  assert.deepEqual(reorderIds(ids, 1, 999, true), ids);
});

test("reorderIds: moving the first item to the end", () => {
  assert.deepEqual(reorderIds([1, 2, 3], 1, 3, true), [2, 3, 1]);
});

test("reorderIds: moving the last item to the front", () => {
  assert.deepEqual(reorderIds([1, 2, 3], 3, 1, false), [3, 1, 2]);
});

test("reorderIds: does not mutate its input", () => {
  const ids = [1, 2, 3];
  const copy = [...ids];
  reorderIds(ids, 1, 3, true);
  assert.deepEqual(ids, copy);
});

test("formatEpisodeLine: zero-pads season and episode numbers", () => {
  assert.equal(
    formatEpisodeLine({ season_number: 1, episode_number: 2, title: "Episode Title" }),
    "S01:E02 - Episode Title",
  );
});

test("formatEpisodeLine: double-digit season/episode numbers are not truncated", () => {
  assert.equal(
    formatEpisodeLine({ season_number: 12, episode_number: 34, title: "Finale" }),
    "S12:E34 - Finale",
  );
});

test("positionRelativeToCurrent: no current item (idle party) is always null", () => {
  assert.equal(positionRelativeToCurrent(0, null), null);
  assert.equal(positionRelativeToCurrent(5, null), null);
});

test("positionRelativeToCurrent: above/current/below", () => {
  assert.equal(positionRelativeToCurrent(0, 2), "above");
  assert.equal(positionRelativeToCurrent(2, 2), "current");
  assert.equal(positionRelativeToCurrent(3, 2), "below");
});
