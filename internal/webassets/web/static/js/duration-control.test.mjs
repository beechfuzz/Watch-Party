// Run with: node --test internal/webassets/web/static/js/duration-control.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import {
  FIELD_CONFIG,
  pluralizeUnit,
  parseTypedSuffix,
  typedNegativeState,
  clampCount,
  sentinelUnitFor,
  restoreUnitOnRaise,
  durationToGoString,
  parseGoDurationString,
} from "./duration-control.js";

const groupA = FIELD_CONFIG.session_idle_timeout; // {-1} ∪ {1..9999}, negativeSentinel "never"
const groupADrift = FIELD_CONFIG.sync_soft_drift; // same shape, millisecond restore unit
const groupB = FIELD_CONFIG.host_grace_period; // -1..9999 contiguous, "no time"/"forever"

test("pluralizeUnit: singular at exactly 1 (and -1), plural otherwise", () => {
  assert.equal(pluralizeUnit("second", 1), "Second");
  assert.equal(pluralizeUnit("second", -1), "Second");
  assert.equal(pluralizeUnit("second", 0), "Seconds");
  assert.equal(pluralizeUnit("second", 2), "Seconds");
  assert.equal(pluralizeUnit("second", 30), "Seconds");
  assert.equal(pluralizeUnit("day", 1), "Day");
  assert.equal(pluralizeUnit("day", 7), "Days");
});

test("pluralizeUnit: unrecognized/sentinel unit words pass through unchanged", () => {
  assert.equal(pluralizeUnit("never", 0), "never");
  assert.equal(pluralizeUnit("forever", -1), "forever");
});

test("parseTypedSuffix: recognizes every mapped suffix letter for a field that offers the unit", () => {
  assert.deepEqual(parseTypedSuffix("5d", groupA), { count: 5, unit: "day" });
  assert.deepEqual(parseTypedSuffix("2w", { ...groupA, suffixUnits: ["week"] }), null); // "w" has no suffix mapping in the source
  assert.deepEqual(parseTypedSuffix("300ms", groupADrift), { count: 300, unit: "millisecond" });
  assert.deepEqual(parseTypedSuffix("300msec", groupADrift), { count: 300, unit: "millisecond" });
  assert.deepEqual(parseTypedSuffix("10sec", groupADrift), { count: 10, unit: "second" });
  assert.deepEqual(parseTypedSuffix("1min", groupADrift), { count: 1, unit: "minute" });
  assert.deepEqual(parseTypedSuffix("2hr", groupADrift), { count: 2, unit: "hour" });
});

test("parseTypedSuffix: a suffix for a unit the field doesn't offer is ignored", () => {
  // Drift fields don't offer "day" per the design source.
  assert.equal(parseTypedSuffix("5d", groupADrift), null);
});

test("parseTypedSuffix: a suffix typed while the numeric value is 0 is ignored", () => {
  assert.equal(parseTypedSuffix("0d", groupA), null);
  assert.equal(parseTypedSuffix("d", groupA), null);
});

test("parseTypedSuffix: no letters at all returns null (caller falls back to plain-number handling)", () => {
  assert.equal(parseTypedSuffix("42", groupA), null);
  assert.equal(parseTypedSuffix("", groupA), null);
});

test("typedNegativeState: does not apply without a leading minus, or on a field with no negative sentinel", () => {
  assert.deepEqual(typedNegativeState("5", groupA), { applies: false });
  assert.deepEqual(typedNegativeState("", groupA), { applies: false });
  assert.deepEqual(typedNegativeState("-5", { ...groupA, negativeSentinel: null }), { applies: false });
});

test("typedNegativeState: a lone leading minus (or minus followed only by 0) is 'pending', not yet committed -- mirrors the design source's own onNumField exactly", () => {
  assert.deepEqual(typedNegativeState("-", groupA), { applies: true, pending: true });
  assert.deepEqual(typedNegativeState("-0", groupA), { applies: true, pending: true });
  assert.deepEqual(typedNegativeState("-", groupB), { applies: true, pending: true });
});

test("typedNegativeState: any other digit after the minus commits immediately to -1 and the field's negative sentinel unit -- live, not just on blur", () => {
  assert.deepEqual(typedNegativeState("-5", groupA), { applies: true, pending: false, count: -1, unit: "never" });
  assert.deepEqual(typedNegativeState("-5", groupB), { applies: true, pending: false, count: -1, unit: "forever" });
  assert.deepEqual(typedNegativeState("-100", groupB), { applies: true, pending: false, count: -1, unit: "forever" });
  assert.deepEqual(typedNegativeState("-05", groupA), { applies: true, pending: false, count: -1, unit: "never" });
});

test("typedNegativeState: applies uniformly across every field that carries a negative sentinel, not just Group B -- both groups now have one per Mark's Round 5 decision", () => {
  for (const field of Object.keys(FIELD_CONFIG)) {
    const config = FIELD_CONFIG[field];
    assert.equal(config.negativeSentinel != null, true, `${field} is expected to carry a negative sentinel`);
    assert.deepEqual(typedNegativeState("-9", config), {
      applies: true,
      pending: false,
      count: -1,
      unit: config.negativeSentinel,
    });
  }
});

test("clampCount: Group B (zeroSentinel !== null) is a plain floor/ceiling, 0 is a normal resting value", () => {
  assert.equal(clampCount(1, -1, groupB), 0);
  assert.equal(clampCount(0, -1, groupB), -1); // floors at -1 (negativeSentinel present)
  assert.equal(clampCount(-1, -1, groupB), -1); // already at floor, stays
  assert.equal(clampCount(0, 1, groupB), 1);
  assert.equal(clampCount(9999, 1, groupB), 9999); // ceiling
  assert.equal(clampCount(-1, 1, groupB), 0); // rising from -1 lands on 0 for Group B -- no gap
});

test("clampCount: Group A (zeroSentinel === null) decrementing from a positive value that would land at/below 0 snaps to -1", () => {
  assert.equal(clampCount(1, -1, groupA), -1);
  assert.equal(clampCount(1, -10, groupA), -1); // accelerated/held step overshooting past 0 still lands exactly on -1
  assert.equal(clampCount(5, -5, groupA), -1);
  assert.equal(clampCount(5, -3, groupA), 2); // doesn't cross the gap -- normal arithmetic
});

test("clampCount: Group A already at -1 stays at -1 when decrementing further (floor)", () => {
  assert.equal(clampCount(-1, -1, groupA), -1);
  assert.equal(clampCount(-1, -100, groupA), -1);
});

test("clampCount: Group A incrementing from -1 always lands on exactly 1, regardless of step size", () => {
  assert.equal(clampCount(-1, 1, groupA), 1);
  assert.equal(clampCount(-1, 5, groupA), 1); // accelerated hold from -1 still lands on 1, not 4
  assert.equal(clampCount(-1, 10, groupA), 1);
});

test("clampCount: Group A incrementing from an already-positive value behaves normally (ceiling at 9999)", () => {
  assert.equal(clampCount(5, 3, groupA), 8);
  assert.equal(clampCount(9995, 10, groupA), 9999);
});

test("sentinelUnitFor: Group A returns the negative sentinel only when count is negative", () => {
  assert.equal(sentinelUnitFor(-1, groupA), "never");
  assert.equal(sentinelUnitFor(1, groupA), null);
  assert.equal(sentinelUnitFor(500, groupA), null);
});

test("sentinelUnitFor: Group B returns the zero sentinel at 0 and the negative sentinel below 0", () => {
  assert.equal(sentinelUnitFor(0, groupB), "none");
  assert.equal(sentinelUnitFor(-1, groupB), "forever");
  assert.equal(sentinelUnitFor(5, groupB), null);
});

test("restoreUnitOnRaise: restores the field's designated unit only when leaving a sentinel unit string", () => {
  assert.equal(restoreUnitOnRaise("never", groupA), "hour"); // session_idle_timeout's own restoreUnit
  assert.equal(restoreUnitOnRaise("day", groupA), "day"); // already a real unit -- unchanged
  assert.equal(restoreUnitOnRaise("forever", groupB), "second");
  assert.equal(restoreUnitOnRaise("none", groupB), "second");
  assert.equal(restoreUnitOnRaise("minute", groupB), "minute");
});

test("restoreUnitOnRaise: the two drift fields restore to millisecond, not second", () => {
  assert.equal(restoreUnitOnRaise("never", groupADrift), "millisecond");
});

test("durationToGoString: direct unit mappings", () => {
  assert.equal(durationToGoString(300, "millisecond", groupADrift), "300ms");
  assert.equal(durationToGoString(10, "second", FIELD_CONFIG.progress_interval), "10s");
  assert.equal(durationToGoString(5, "minute", FIELD_CONFIG.progress_interval), "5m");
  assert.equal(durationToGoString(2, "hour", groupA), "2h");
});

test("durationToGoString: Day/Week/Month convert to hours (Go has no such unit)", () => {
  assert.equal(durationToGoString(1, "day", groupA), "24h");
  assert.equal(durationToGoString(1, "week", groupA), "168h");
  assert.equal(durationToGoString(1, "month", FIELD_CONFIG.session_age_timeout), "720h");
  assert.equal(durationToGoString(2, "week", groupA), "336h");
});

test("durationToGoString: both sentinel kinds serialize as real, parseable Go duration strings", () => {
  assert.equal(durationToGoString(-1, "never", groupA), "-1s");
  assert.equal(durationToGoString(-1, "forever", groupB), "-1s");
  assert.equal(durationToGoString(0, "none", groupB), "0s");
});

test("parseGoDurationString: round-trips a plain unit string", () => {
  assert.deepEqual(parseGoDurationString("300ms", groupADrift), { count: 300, unit: "millisecond" });
  assert.deepEqual(parseGoDurationString("10s", FIELD_CONFIG.progress_interval), { count: 10, unit: "second" });
});

test("parseGoDurationString: collapses whole-hours into the field's largest evenly-dividing unit", () => {
  assert.deepEqual(parseGoDurationString("24h", groupA), { count: 1, unit: "day" });
  assert.deepEqual(parseGoDurationString("168h", groupA), { count: 1, unit: "week" });
  assert.deepEqual(parseGoDurationString("720h", FIELD_CONFIG.session_age_timeout), { count: 1, unit: "month" });
  assert.deepEqual(parseGoDurationString("2h", groupA), { count: 2, unit: "hour" }); // doesn't divide evenly into day/week
});

test("parseGoDurationString: sentinel strings map back to the field's sentinel unit", () => {
  assert.deepEqual(parseGoDurationString("-1s", groupA), { count: -1, unit: "never" });
  assert.deepEqual(parseGoDurationString("-1s", groupB), { count: -1, unit: "forever" });
  assert.deepEqual(parseGoDurationString("0s", groupB), { count: 0, unit: "none" });
});

test("parseGoDurationString: an unparseable or empty string falls back to the field's own zero/negative/first unit", () => {
  assert.deepEqual(parseGoDurationString("", groupB), { count: 0, unit: "none" });
  assert.deepEqual(parseGoDurationString("garbage", groupA), { count: 0, unit: "never" });
});
