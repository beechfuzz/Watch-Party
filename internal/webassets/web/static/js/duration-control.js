// Pure logic behind the setup wizard's duration spin-box + unit-select
// control (setup.html/wizard-steps.js) -- no DOM, so it can be unit tested
// directly (duration-control.test.mjs), the same split as
// playlist-select.js's selection math. One shared implementation drives all
// 9 duration fields across the wizard's Server/Party/Playback-sync steps,
// parameterized per field by a FIELD_CONFIG entry.
//
// Two sentinel shapes exist, per Mark's Round 5 decision (see
// ARCHITECTURE.md's setup wizard section):
//   - Group A (session idle/max-age, Emby progress interval, sync snapshot
//     interval, soft/hard drift): 0 is not a value these fields can hold at
//     all. The number line is {-1} ∪ {1..9999} -- a single-value gap at
//     0. -1 ("Never") is the sole disable sentinel.
//   - Group B (host grace period, party inactivity timeout): 0 ("No Time")
//     is a normal resting value meaning "trigger immediately"; -1
//     ("Forever") disables the feature. No gap -- the range is contiguous
//     from -1 up to 9999.
// zeroSentinel === null marks a Group A config; a non-null zeroSentinel
// marks Group B.

export const MAX_DURATION_COUNT = 9999;

// Field configs. restoreUnit is derived per field from that field's own
// first real (non-sentinel) unit -- NOT a single global "second vs
// millisecond" rule. The source prototype's own sentinelUnit() always
// restores to a hardcoded 'second' for every non-drift field, including
// Session Idle Timeout / Session Max Age, whose own unit dropdowns don't
// offer "Second" at all (they're Hour/Day/Week(/Month)-only) -- reusing
// that literally would leave a real <select> with no matching <option>
// after raising off the disable sentinel. Restoring to each field's own
// first listed unit instead sidesteps that gap while keeping every other
// field's restore target exactly what the source already used.
export const FIELD_CONFIG = {
  session_idle_timeout: {
    units: ["hour", "day", "week"],
    zeroSentinel: null,
    negativeSentinel: "never",
    restoreUnit: "hour",
    suffixUnits: ["hour", "day", "week"],
  },
  session_age_timeout: {
    units: ["hour", "day", "week", "month"],
    zeroSentinel: null,
    negativeSentinel: "never",
    restoreUnit: "hour",
    suffixUnits: ["hour", "day", "week"], // "month" has no typed-suffix letter in the source's map
  },
  progress_interval: {
    units: ["second", "minute", "hour", "day"],
    zeroSentinel: null,
    negativeSentinel: "never",
    restoreUnit: "second",
    suffixUnits: ["second", "minute", "hour", "day"],
  },
  sync_snapshot_interval: {
    units: ["second", "minute", "hour", "day"],
    zeroSentinel: null,
    negativeSentinel: "never",
    restoreUnit: "second",
    suffixUnits: ["second", "minute", "hour", "day"],
  },
  sync_soft_drift: {
    units: ["millisecond", "second", "minute", "hour"],
    zeroSentinel: null,
    negativeSentinel: "never",
    restoreUnit: "millisecond",
    suffixUnits: ["millisecond", "second", "minute", "hour"], // no "day" -- drift fields don't offer it
  },
  sync_hard_drift: {
    units: ["millisecond", "second", "minute", "hour"],
    zeroSentinel: null,
    negativeSentinel: "never",
    restoreUnit: "millisecond",
    suffixUnits: ["millisecond", "second", "minute", "hour"],
  },
  host_grace_period: {
    units: ["second", "minute", "hour", "day"],
    zeroSentinel: "none",
    negativeSentinel: "forever",
    restoreUnit: "second",
    suffixUnits: ["second", "minute", "hour", "day"],
  },
  inactivity_timeout: {
    units: ["second", "minute", "hour", "day"],
    zeroSentinel: "none",
    negativeSentinel: "forever",
    restoreUnit: "second",
    suffixUnits: ["second", "minute", "hour", "day"],
  },
};

const UNIT_LABELS = {
  millisecond: ["Millisecond", "Milliseconds"],
  second: ["Second", "Seconds"],
  minute: ["Minute", "Minutes"],
  hour: ["Hour", "Hours"],
  day: ["Day", "Days"],
  week: ["Week", "Weeks"],
  month: ["Month", "Months"],
};

// Typed-suffix letters -> unit, matching the design source's map exactly
// (ms/msec, s/sec, m/min, h/hr, d). Filtered per field by
// FIELD_CONFIG[field].suffixUnits before being applied -- a suffix for a
// unit the field doesn't offer (e.g. "d" on a drift field) is ignored.
const SUFFIX_MAP = {
  ms: "millisecond",
  msec: "millisecond",
  s: "second",
  sec: "second",
  m: "minute",
  min: "minute",
  h: "hour",
  hr: "hour",
  d: "day",
};

// Singular at exactly 1 (in magnitude -- -1 "Forever"/"Never" are sentinel
// units, never passed here), plural otherwise. Unrecognized unit words pass
// through unchanged rather than throwing, since a sentinel unit string
// (never/none/forever) is never a real duration unit to pluralize.
export function pluralizeUnit(unitWord, count) {
  const pair = UNIT_LABELS[unitWord];
  if (!pair) return unitWord;
  const n = typeof count === "string" ? parseInt(count, 10) : count;
  return Math.abs(n) === 1 ? pair[0] : pair[1];
}

// Parses a raw typed field value (e.g. "5d", "-5", "300ms") for a suffix
// letter run, per the design source's blur-time behavior: letters are
// stripped, mapped to a unit via SUFFIX_MAP, and only accepted if that unit
// is one this field actually offers (config.suffixUnits) and the numeric
// part isn't 0 (a suffix typed while the value is 0 is ignored, exactly as
// documented). Returns null when there's no suffix to apply -- the caller
// should fall back to treating raw as a plain number.
export function parseTypedSuffix(raw, config) {
  const letters = raw.replace(/[^a-zA-Z]/g, "").toLowerCase();
  if (!letters) return null;
  const digits = raw.replace(/[^0-9]/g, "");
  const count = digits === "" ? 0 : parseInt(digits, 10);
  if (count === 0) return null;
  const unit = SUFFIX_MAP[letters] || SUFFIX_MAP[letters.slice(-2)] || SUFFIX_MAP[letters.slice(-1)];
  if (!unit || !config.suffixUnits.includes(unit)) return null;
  return { count: Math.min(MAX_DURATION_COUNT, count), unit };
}

// Live, per-keystroke read of a leading minus sign, mirroring the design
// source's onNumField exactly: verified directly against the source (not
// just the README) that typing a negative value clamps to the disable
// sentinel immediately, on every keystroke, not only on blur. A bare
// leading minus with no digits after it yet -- or "-0" -- is still
// "pending": the source keeps the field showing a literal "-" rather than
// committing early, so the operator can keep typing more digits. Any other
// digit after the minus commits immediately to the sentinel. Returns
// `{applies: false}` when the field has no negative sentinel at all or raw
// has no leading minus, meaning the caller's normal (non-negative) typed-
// value handling applies instead.
export function typedNegativeState(raw, config) {
  if (!config.negativeSentinel || !/^\s*-/.test(raw)) return { applies: false };
  const digits = raw.replace(/[^0-9]/g, "");
  if (digits === "" || digits === "0") return { applies: true, pending: true };
  return { applies: true, pending: false, count: -1, unit: config.negativeSentinel };
}

// Steps cur by delta and returns the new count, honoring each config's
// number-line shape:
//   - Group B (zeroSentinel !== null): plain floor/ceiling, 0 <= n <= 9999,
//     or -1 <= n <= 9999 when a negative sentinel exists. 0 is a normal
//     resting value.
//   - Group A (zeroSentinel === null): the number line has a single-value
//     gap at 0 -- {-1} ∪ {1..9999}. Any decrement that would land at or
//     below 0 snaps straight to -1 (discarding overshoot magnitude, the
//     same way the existing floor already discards how far past a boundary
//     a big step would have gone); any increment starting from -1 (or, by
//     construction, anywhere at or below 0) always lands on exactly 1,
//     regardless of the step's size -- crossing the gap costs the whole
//     step, it doesn't partially refund it on the other side.
export function clampCount(cur, delta, config) {
  const floor = config.zeroSentinel !== null ? (config.negativeSentinel ? -1 : 0) : null;
  if (floor !== null) {
    return Math.min(MAX_DURATION_COUNT, Math.max(floor, cur + delta));
  }
  if (delta === 0) return cur;
  if (delta < 0) {
    const next = cur + delta;
    return next <= 0 ? -1 : next;
  }
  // delta > 0
  if (cur <= 0) return 1;
  return Math.min(MAX_DURATION_COUNT, cur + delta);
}

// Returns the sentinel unit count currently resolves to (config's
// zeroSentinel at 0, negativeSentinel at any negative value), or null if
// count isn't at a sentinel -- the caller should keep whatever real unit is
// already selected in that case (see restoreUnitOnRaise for the one
// remaining case: transitioning OUT of a sentinel).
export function sentinelUnitFor(count, config) {
  if (config.negativeSentinel && count < 0) return config.negativeSentinel;
  if (config.zeroSentinel !== null && count === 0) return config.zeroSentinel;
  return null;
}

// When count is no longer at a sentinel value but the field's currently
// selected unit still IS a sentinel unit string (i.e. the count just raised
// off 0/-1), restore config.restoreUnit; otherwise leave previousUnit
// untouched. Combined with sentinelUnitFor, this is the field's full unit
// transition: `sentinelUnitFor(count, config) ?? restoreUnitOnRaise(prevUnit, config)`.
export function restoreUnitOnRaise(previousUnit, config) {
  const sentinelUnits = [config.zeroSentinel, config.negativeSentinel].filter((u) => u != null);
  return sentinelUnits.includes(previousUnit) ? config.restoreUnit : previousUnit;
}

// Canonical Go duration-string output for the review step and config
// preview -- never the two-part spin+unit form. Day/Week/Month have no Go
// duration unit, so they convert to hours (×24, ×168, ×720). Both sentinel
// kinds always serialize as a real, config.ParseDuration-parseable string
// (never the literal word "never"; see ARCHITECTURE.md's setup wizard
// section on why): "0s" for Group B's No Time, "-1s" for either group's
// disable sentinel (Never or Forever).
// Inverse of durationToGoString -- parses a Go duration string (as stored
// in the hidden true-value input, seeded from the server's default or a
// re-rendered submission) back into {count, unit} to initialize or restore
// a spin box's displayed state. Collapses a whole-hours value into the
// field's largest configured unit that divides it evenly (so e.g. "720h"
// displays as "1 Month" rather than "720 Hours") purely for a nicer
// initial label; the canonical stored/submitted value is always what
// durationToGoString produces from the resulting count+unit, never this
// function's input string verbatim.
export function parseGoDurationString(raw, config) {
  const fallbackUnit = config.zeroSentinel ?? config.negativeSentinel ?? config.units[0];
  const s = String(raw ?? "").trim();
  const m = s.match(/^(-?\d+)(ms|s|m|h)$/);
  if (!m) return { count: 0, unit: fallbackUnit };
  const n = parseInt(m[1], 10);
  const unitChar = m[2];
  const rawUnit = { ms: "millisecond", s: "second", m: "minute", h: "hour" }[unitChar];

  if (n < 0) return { count: -1, unit: config.negativeSentinel || rawUnit };
  if (n === 0) return { count: 0, unit: config.zeroSentinel !== null ? config.zeroSentinel : rawUnit };

  if (rawUnit === "hour") {
    if (config.units.includes("month") && n % 720 === 0) return { count: n / 720, unit: "month" };
    if (config.units.includes("week") && n % 168 === 0) return { count: n / 168, unit: "week" };
    if (config.units.includes("day") && n % 24 === 0) return { count: n / 24, unit: "day" };
  }
  const unit = config.units.includes(rawUnit) ? rawUnit : config.units[0];
  return { count: n, unit };
}

export function durationToGoString(count, unit, config) {
  if (config.zeroSentinel !== null && unit === config.zeroSentinel) return "0s";
  if (config.negativeSentinel && unit === config.negativeSentinel) return "-1s";
  switch (unit) {
    case "millisecond":
      return `${count}ms`;
    case "second":
      return `${count}s`;
    case "minute":
      return `${count}m`;
    case "hour":
      return `${count}h`;
    case "day":
      return `${count * 24}h`;
    case "week":
      return `${count * 168}h`;
    case "month":
      return `${count * 720}h`;
    default:
      return `${count}s`;
  }
}
