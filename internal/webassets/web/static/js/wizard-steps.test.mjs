// Run with: node --test internal/webassets/web/static/js/wizard-steps.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import {
  REQUIRED_FIELDS,
  requiredFieldsMissing,
  firstStepWithMissingField,
  canNavigateToStep,
  nextMaxSeen,
  railItemState,
  filterReviewGroups,
  buildConfigPreview,
  parseOrigins,
  driftOrderValid,
  rateInRange,
  clampPort,
  reassembleListenAddress,
} from "./wizard-steps.js";

const fullValues = () => ({
  title: "Watch Party",
  browser_origins: "https://watchparty.example.com",
  server_url: "http://emby:8096",
});

test("REQUIRED_FIELDS includes server_url alongside title/browser_origins (Round 4 §6, confirmed)", () => {
  assert.deepEqual([...REQUIRED_FIELDS].sort(), ["browser_origins", "server_url", "title"]);
});

test("requiredFieldsMissing: empty when every required field is non-blank", () => {
  assert.deepEqual(requiredFieldsMissing(fullValues()), []);
});

test("requiredFieldsMissing: reports blank and whitespace-only fields", () => {
  assert.deepEqual(requiredFieldsMissing({ ...fullValues(), title: "" }), ["title"]);
  assert.deepEqual(requiredFieldsMissing({ ...fullValues(), title: "   " }), ["title"]);
  assert.deepEqual(requiredFieldsMissing({ ...fullValues(), browser_origins: "", server_url: "" }).sort(), [
    "browser_origins",
    "server_url",
  ]);
});

test("requiredFieldsMissing: a missing key (undefined) counts as blank", () => {
  assert.deepEqual(requiredFieldsMissing({ title: "x", browser_origins: "y" }), ["server_url"]);
});

test("firstStepWithMissingField: step 1 when nothing is missing", () => {
  assert.equal(firstStepWithMissingField([]), 1);
});

test("firstStepWithMissingField: the lowest-numbered step holding any missing field -- fixes the trap where a fresh load with blank browser_origins/server_url (both step 2, no sensible default) would otherwise leave the operator stuck on step 1 with no visible offending field and every rail item/Next disabled by the same gate", () => {
  assert.equal(firstStepWithMissingField(["browser_origins"]), 2);
  assert.equal(firstStepWithMissingField(["server_url", "browser_origins"]), 2);
  assert.equal(firstStepWithMissingField(["title"]), 2);
});

test("firstStepWithMissingField: takes the minimum across multiple missing fields on different steps", () => {
  const fieldStep = { a: 3, b: 1, c: 2 };
  assert.equal(firstStepWithMissingField(["a", "c"], fieldStep), 2);
  assert.equal(firstStepWithMissingField(["a", "b", "c"], fieldStep), 1);
});

test("canNavigateToStep: allowed when nothing is missing and the target isn't the current step", () => {
  assert.equal(canNavigateToStep(4, 1, []), true); // skip-ahead, nothing visited between
  assert.equal(canNavigateToStep(1, 4, []), true); // skip-back
});

test("canNavigateToStep: refused when something required is missing, regardless of target", () => {
  assert.equal(canNavigateToStep(4, 1, ["title"]), false);
  assert.equal(canNavigateToStep(2, 1, ["title"]), false);
});

test("canNavigateToStep: refused for the already-current step even with nothing missing", () => {
  assert.equal(canNavigateToStep(2, 2, []), false);
});

test("nextMaxSeen: only ever grows, to the target step", () => {
  assert.equal(nextMaxSeen(1, 3), 3); // skip-ahead raises maxSeen straight to 3
  assert.equal(nextMaxSeen(3, 1), 3); // back navigation doesn't lower it
  assert.equal(nextMaxSeen(2, 2), 2);
});

test("railItemState: current for the viewed step", () => {
  assert.equal(railItemState(2, 2, 4), "current");
});

test("railItemState: done for any step below maxSeen that isn't current -- including a step skipped over, not just one actually visited", () => {
  // Skipping 1 -> 4 directly raises maxSeen to 4; steps 2 and 3 were never
  // viewed but must still show "done" (checkmarked), per the design
  // source's rail onClick -- this is deliberate, not a gap.
  assert.equal(railItemState(2, 4, 4), "done");
  assert.equal(railItemState(3, 4, 4), "done");
});

test("railItemState: upcoming for anything at or beyond maxSeen that isn't current", () => {
  assert.equal(railItemState(3, 1, 1), "upcoming");
  assert.equal(railItemState(4, 2, 2), "upcoming");
});

test("filterReviewGroups: groups fields by their configured group, in FIELD_META order, advanced rows included when advancedOn", () => {
  const values = {
    title: "Watch Party",
    log_level: "info",
    server_url: "http://emby:8096",
    session_idle_timeout: "24h",
  };
  const groups = filterReviewGroups(values, true);
  const serverGroup = groups.find((g) => g.title === "Server");
  assert.ok(serverGroup, "expected a Server group");
  const fields = serverGroup.rows.map((r) => r.field);
  assert.ok(fields.includes("title") && fields.includes("session_idle_timeout"));
});

test("filterReviewGroups: advanced rows are dropped entirely when advancedOn is false", () => {
  const values = { session_idle_timeout: "24h" };
  const groups = filterReviewGroups(values, false);
  assert.deepEqual(groups, []); // the only value supplied is advanced-only
});

test("filterReviewGroups: a group whose every row is advanced disappears entirely with Advanced off (Playback Sync)", () => {
  const values = {
    title: "Watch Party", // Server group, non-advanced -- should still appear
    progress_interval: "10s",
    sync_snapshot_interval: "4s",
    sync_soft_drift: "300ms",
    sync_hard_drift: "1500ms",
    sync_max_rate_adjustment: "0.05",
  };
  const groupsOff = filterReviewGroups(values, false);
  assert.equal(
    groupsOff.find((g) => g.title === "Playback Sync"),
    undefined,
    "Playback Sync is entirely advanced -- must vanish with Advanced off"
  );
  assert.ok(groupsOff.find((g) => g.title === "Server"), "Server group has a non-advanced row and must remain");

  const groupsOn = filterReviewGroups(values, true);
  assert.ok(groupsOn.find((g) => g.title === "Playback Sync"), "Playback Sync must reappear with Advanced on");
});

test("filterReviewGroups: only fields actually present in values are rendered", () => {
  const groups = filterReviewGroups({ title: "Watch Party" }, true);
  const allRows = groups.flatMap((g) => g.rows);
  assert.equal(allRows.length, 1);
  assert.equal(allRows[0].field, "title");
});

test("buildConfigPreview: renders string/array/number fields with the right JSON shapes and key order", () => {
  const preview = buildConfigPreview({
    title: "Watch Party",
    browser_origins: ["https://a.example.com", "https://b.example.com"],
    sync_max_rate_adjustment: 0.05,
  });
  assert.match(preview, /"title": "Watch Party",/);
  assert.match(preview, /"browser_origins": \["https:\/\/a\.example\.com", "https:\/\/b\.example\.com"\],/);
  assert.match(preview, /"sync_max_rate_adjustment": 0\.05/);
  // title comes before browser_origins in FIELD_META order
  assert.ok(preview.indexOf('"title"') < preview.indexOf('"browser_origins"'));
});

test("buildConfigPreview: no trailing comma on the last key", () => {
  const preview = buildConfigPreview({ public_url: "http://emby:8096" }); // last key in FIELD_META order among a 1-entry set
  const lines = preview.split("\n");
  const keyLine = lines.find((l) => l.includes("public_url"));
  assert.ok(!keyLine.trim().endsWith(","));
});

test("parseOrigins: splits on newlines and commas, trims, drops empties, strips trailing slash", () => {
  assert.deepEqual(parseOrigins("https://a.example.com/\nhttps://b.example.com, https://c.example.com\n\n"), [
    "https://a.example.com",
    "https://b.example.com",
    "https://c.example.com",
  ]);
});

test("driftOrderValid: requires hard > soft when both are non-negative (mirrors config.ValidateSyncDrift)", () => {
  assert.equal(driftOrderValid(300, 1500), true);
  assert.equal(driftOrderValid(300, 300), false);
  assert.equal(driftOrderValid(1500, 300), false);
});

test("driftOrderValid: exempt when either side is negative (Round 5 §9.2 -- disabled isn't orderable)", () => {
  assert.equal(driftOrderValid(-1, 1500), true);
  assert.equal(driftOrderValid(300, -1), true);
  assert.equal(driftOrderValid(-1, -1), true);
  assert.equal(driftOrderValid(-1, -2), true);
});

test("rateInRange: strictly between 0 and 1", () => {
  assert.equal(rateInRange(0.05), true);
  assert.equal(rateInRange(0), false);
  assert.equal(rateInRange(1), false);
  assert.equal(rateInRange(-0.1), false);
  assert.equal(rateInRange(1.5), false);
});

test("clampPort: digits only, clamped to 1-65535, blank falls back to 8080", () => {
  assert.equal(clampPort("8080"), 8080);
  assert.equal(clampPort("abc8080xyz"), 8080);
  assert.equal(clampPort("0"), 1);
  assert.equal(clampPort("99999999"), 65535);
  assert.equal(clampPort(""), 8080);
});

test("reassembleListenAddress: joins ip:port, blank ip falls back to 0.0.0.0", () => {
  assert.equal(reassembleListenAddress("0.0.0.0", 8080), "0.0.0.0:8080");
  assert.equal(reassembleListenAddress("", 8080), "0.0.0.0:8080");
  assert.equal(reassembleListenAddress("  ", 8080), "0.0.0.0:8080");
  assert.equal(reassembleListenAddress("127.0.0.1", 9090), "127.0.0.1:9090");
});
