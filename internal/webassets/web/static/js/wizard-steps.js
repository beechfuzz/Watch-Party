// Pure step-navigation/gating, Show-Advanced filtering, and config-preview
// logic behind the setup wizard (setup.html) -- no DOM, so it can be unit
// tested directly (wizard-steps.test.mjs), the same split as
// playlist-select.js's selection math and duration-control.js's spin-box
// math. wireWizardSteps() at the bottom is the thin, untested DOM layer
// that reads/writes the real form and calls these.

import {
  FIELD_CONFIG,
  durationToGoString,
  parseGoDurationString,
  clampCount,
  sentinelUnitFor,
  restoreUnitOnRaise,
  parseTypedSuffix,
  typedNegativeState,
  pluralizeUnit,
} from "./duration-control.js";

// Required fields per Mark's confirmation on Round 4 §6's recommendation:
// title and browser_origins are server-required with no default
// (config.ValidateTitle / config.ValidateOrigins); server_url is also
// server-required (config.resolveStringRequired has no fallback) but the
// design package left it out of its own pulsing/gating set, relying
// instead on blur-to-default. Added here as agreed defense-in-depth so the
// wizard's own gating matches everything the server actually requires, not
// just what the design mockup happened to pulse.
//
// As of ARCHITECTURE.md §16.20, browser_origins/server_url ship with a
// real wizard-only suggested value on load (setup_wizard.go's
// defaultSetupFieldValues) rather than blank -- internal/config.loadFrom
// itself still has no fallback for either, so this list and the gating
// below are unchanged and still needed: an operator who clears one back to
// blank must still be blocked exactly as before.
export const REQUIRED_FIELDS = ["title", "browser_origins", "server_url"];

export function requiredFieldsMissing(values) {
  return REQUIRED_FIELDS.filter((k) => String(values[k] ?? "").trim() === "");
}

// Which step each required field's input lives on -- all three (title,
// browser_origins, server_url) are on step 2 today, but this is written
// generally rather than hardcoded to that fact.
export const FIELD_STEP = { title: 2, browser_origins: 2, server_url: 2 };

// Where the wizard should open given a set of currently-missing required
// fields: the lowest-numbered step holding one of them, or step 1 if
// nothing is missing. Needed because the required-field gate is global
// (blocks Next/Back/every rail item, not just the step the field lives
// on) -- originally written because browser_origins/server_url had no
// sensible site-generic default (unlike title) and so a fresh page load
// could start with fields already missing, landing the operator on step 1
// with no offending field anywhere on it and no way to reach the step that
// needs attention (Next and every rail item, including the one for the
// step that actually holds the blank field, disabled by that same gate).
// As of ARCHITECTURE.md §16.20, browser_origins/server_url now ship with a
// real (if operator-decided, wizard-only) suggested default too, so a
// fresh load normally has nothing missing and this now simply returns 1 --
// but the function itself is unchanged and still the correct thing to call
// for the cases where something genuinely is missing: an operator who
// clears a required field back to blank, or a server-side validation-error
// re-render. This mirrors the same "open the step that needs attention"
// idea Round 3's plan already anticipated for that server-side re-render
// case (there, driven by which fields the server rejected; here, by
// whichever required fields are actually blank right now).
export function firstStepWithMissingField(missing, fieldStep = FIELD_STEP) {
  if (missing.length === 0) return 1;
  return Math.min(...missing.map((f) => fieldStep[f] ?? 1));
}

// A rail click (or Next/Back) is allowed exactly when nothing required is
// missing and the target isn't already the current step. Per the design
// source's own rail onClick (not its otherwise-unused goTo() helper --
// see ARCHITECTURE.md's setup wizard notes), there is deliberately no
// maxSeen/skip-ahead restriction: when nothing is missing, every rail item
// is clickable, including steps never visited.
export function canNavigateToStep(targetStep, currentStep, missing) {
  return missing.length === 0 && targetStep !== currentStep;
}

// maxSeen only ever grows, to the highest step directly reached by any
// successful transition (Next, Back, or a rail click) -- it drives the
// rail's checkmark badge, never navigability itself (see
// canNavigateToStep). A rail click that skips steps still raises maxSeen
// to the clicked step, marking the skipped steps "done" even though their
// content was never viewed -- ported deliberately, not a gap: see the
// design source's rail onClick, which computes exactly this.
export function nextMaxSeen(maxSeen, targetStep) {
  return Math.max(maxSeen, targetStep);
}

// "current" for the step being viewed, "done" for any step below the
// highest reached (maxSeen), "upcoming" otherwise. The source's rail
// state also OR's in `st.n < s.current`, which is provably redundant given
// the invariant maxSeen >= current (maxSeen only ever advances to
// current's new value) -- dropped here since there's nothing live left for
// it to do.
export function railItemState(n, current, maxSeen) {
  if (n === current) return "current";
  return n < maxSeen ? "done" : "upcoming";
}

// Field -> {group, label, advanced} for the Review step and config
// preview. Order within FIELD_META is the review/preview row order (also
// config.jsonc's own key order, per the design source).
export const FIELD_META = {
  title: { group: "Server", label: "Site Title", advanced: false },
  log_level: { group: "Server", label: "Log Level", advanced: false },
  browser_origins: { group: "Server", label: "Browser Origins", advanced: false },
  listen_address: { group: "Server", label: "Listen Address", advanced: false },
  session_idle_timeout: { group: "Server", label: "Session Idle Timeout", advanced: true },
  session_age_timeout: { group: "Server", label: "Session Max Age", advanced: true },
  server_url: { group: "Media Server", label: "Emby Server URL", advanced: false },
  public_url: { group: "Media Server", label: "Emby Public URL", advanced: false },
  host_grace_period: { group: "Party", label: "Host Grace Period", advanced: false },
  inactivity_timeout: { group: "Party", label: "Party Inactivity Timeout", advanced: false },
  progress_interval: { group: "Playback Sync", label: "Emby Progress Report Interval", advanced: true },
  sync_snapshot_interval: { group: "Playback Sync", label: "Sync Snapshot Interval", advanced: true },
  sync_soft_drift: { group: "Playback Sync", label: "Soft Drift Threshold", advanced: true },
  sync_hard_drift: { group: "Playback Sync", label: "Hard Drift Threshold", advanced: true },
  sync_max_rate_adjustment: { group: "Playback Sync", label: "Max Playback Rate Adjustment", advanced: true },
};

const GROUP_ORDER = ["Server", "Media Server", "Party", "Playback Sync"];

// values holds every field's *display* value already resolved to a plain
// string (durations already converted via durationToGoString by the
// caller, browser_origins already comma-joined, listen_address already
// "ip:port", public_url already substituted with "(same as server URL)"
// when blank) -- this function only groups/filters/orders, it doesn't
// know how to format any individual field.
export function filterReviewGroups(values, advancedOn) {
  const groups = GROUP_ORDER.map((title) => ({ title, rows: [] }));
  for (const [field, meta] of Object.entries(FIELD_META)) {
    if (meta.advanced && !advancedOn) continue;
    if (!(field in values)) continue;
    const group = groups.find((g) => g.title === meta.group);
    group.rows.push({ field, label: meta.label, value: values[field], advanced: meta.advanced });
  }
  return groups.filter((g) => g.rows.length > 0);
}

// The advanced-only config.jsonc preview block: a best-effort visual
// approximation (not shared/duplicated templating logic against
// internal/config/templates/config.jsonc.tmpl -- see the review step's own
// "Preview -- approximate" label). previewValues maps field -> the raw
// value to render (a string, an array for browser_origins, or a number for
// sync_max_rate_adjustment); key order matches FIELD_META's.
export function buildConfigPreview(previewValues) {
  const lines = ["{"];
  const keys = Object.keys(FIELD_META).filter((k) => k in previewValues);
  keys.forEach((key, i) => {
    const v = previewValues[key];
    const comma = i < keys.length - 1 ? "," : "";
    let rendered;
    if (Array.isArray(v)) {
      rendered = `[${v.map((s) => JSON.stringify(s)).join(", ")}]`;
    } else if (typeof v === "number") {
      rendered = String(v);
    } else {
      rendered = JSON.stringify(String(v));
    }
    lines.push(`  ${JSON.stringify(key)}: ${rendered}${comma}`);
  });
  lines.push("}");
  return lines.join("\n");
}

// --- Client-side validation mirrors (responsiveness only -- see
// internal/config's Validate*/Parse* functions, the sole authority). ---

export function parseOrigins(raw) {
  return raw
    .split(/[\n,]/)
    .map((s) => s.trim().replace(/\/+$/, ""))
    .filter(Boolean);
}

// Mirrors config.ValidateSyncDrift's disabled-side exemption (Round 5
// §9.2): ordering between "disabled" (negative) and anything else,
// including another disabled value, isn't meaningful, so the check is
// skipped whenever either side is negative.
export function driftOrderValid(softMS, hardMS) {
  if (softMS < 0 || hardMS < 0) return true;
  return hardMS > softMS;
}

export function rateInRange(v) {
  return v > 0 && v < 1;
}

export function clampPort(raw) {
  const digits = String(raw).replace(/[^0-9]/g, "");
  if (digits === "") return 8080;
  return Math.min(65535, Math.max(1, parseInt(digits, 10)));
}

export function reassembleListenAddress(ip, port) {
  return `${String(ip).trim() || "0.0.0.0"}:${port}`;
}

// --- DOM wiring (untested, thin) ---

// wireWizardSteps attaches every interaction the wizard needs to a real
// DOM built by setup.html: step paging, the rail, Show Advanced, the 9
// duration controls, listen-address reassembly, required-field
// pulsing/gating, and the review/preview render. ids names every element
// this needs by id (see setup.html for the exact ids used).
export function wireWizardSteps(doc) {
  const root = doc.getElementById("wizard-root");
  if (!root) return; // not on this page (e.g. the post-save success view)

  const state = {
    current: 1,
    maxSeen: 1,
    adv: false,
  };

  const railButtons = Array.from(doc.querySelectorAll("[data-rail-step]"));
  const stepSections = Array.from(doc.querySelectorAll("[data-step]"));
  const nextBtn = doc.getElementById("wizard-next");
  const backBtn = doc.getElementById("wizard-back");
  const saveBtn = doc.getElementById("wizard-save");
  const advToggle = doc.getElementById("wizard-advanced-toggle");
  const form = doc.getElementById("wizard-form");

  function fieldValues() {
    const values = {};
    for (const el of form.elements) {
      if (!el.name) continue;
      values[el.name] = el.value;
    }
    return values;
  }

  function render() {
    const missing = requiredFieldsMissing(fieldValues());
    for (const btn of railButtons) {
      const n = parseInt(btn.dataset.railStep, 10);
      const st = railItemState(n, state.current, state.maxSeen);
      btn.dataset.state = st;
      const clickable = canNavigateToStep(n, state.current, missing);
      btn.classList.toggle("is-blocked", !clickable && st !== "current");
    }
    for (const section of stepSections) {
      section.hidden = parseInt(section.dataset.step, 10) !== state.current;
    }
    const blocked = missing.length > 0;
    if (backBtn) backBtn.disabled = state.current === 1 || blocked;
    if (nextBtn) {
      nextBtn.disabled = blocked;
      nextBtn.hidden = state.current === 4;
    }
    if (saveBtn) saveBtn.hidden = state.current !== 4;
    for (const name of REQUIRED_FIELDS) {
      const el = form.elements.namedItem(name);
      if (el) el.classList.toggle("is-required-pulse", missing.includes(name));
    }
    doc.body.classList.toggle("wizard-advanced-on", state.adv);
    if (state.current === 4) renderReview();
  }

  function goTo(n) {
    const missing = requiredFieldsMissing(fieldValues());
    if (!canNavigateToStep(n, state.current, missing)) return;
    state.current = n;
    state.maxSeen = nextMaxSeen(state.maxSeen, n);
    render();
  }

  for (const btn of railButtons) {
    btn.addEventListener("click", () => goTo(parseInt(btn.dataset.railStep, 10)));
  }
  if (nextBtn) nextBtn.addEventListener("click", () => goTo(Math.min(4, state.current + 1)));
  if (backBtn) backBtn.addEventListener("click", () => goTo(Math.max(1, state.current - 1)));
  if (advToggle) {
    advToggle.addEventListener("click", () => {
      state.adv = !state.adv;
      advToggle.setAttribute("aria-checked", String(state.adv));
      render();
    });
  }
  form.addEventListener("input", render);

  for (const field of Object.keys(FIELD_CONFIG)) {
    wireDurationField(doc, form, field, FIELD_CONFIG[field], render);
  }
  wireListenAddress(doc, form, render);
  wireRateField(doc, form, render);
  wireDisclosure(doc);

  function renderReview() {
    const previewEl = doc.getElementById("wizard-preview");
    const previewSection = doc.getElementById("wizard-preview-section");
    const reviewEl = doc.getElementById("wizard-review");
    if (!reviewEl) return;

    const values = fieldValues();
    const display = {};
    const previewValues = {};
    for (const field of Object.keys(FIELD_META)) {
      const cfg = FIELD_CONFIG[field];
      if (cfg) {
        const hiddenEl = form.elements.namedItem(field);
        const goStr = hiddenEl ? hiddenEl.value : durationToGoString(0, cfg.units[0], cfg);
        display[field] = goStr;
        previewValues[field] = goStr;
      } else if (field === "browser_origins") {
        const origins = parseOrigins(values.browser_origins || "");
        display[field] = origins.join(", ");
        previewValues[field] = origins;
      } else if (field === "listen_address") {
        display[field] = values.listen_address || "";
        previewValues[field] = values.listen_address || "";
      } else if (field === "public_url") {
        // "(same as server URL)" triggers on value-equality, not just
        // blankness (ARCHITECTURE.md §16.20): public_url now ships
        // defaulted to the same wizard-only suggested value as server_url,
        // so an operator who edits neither would otherwise see two
        // identical, unannotated URLs here instead of the "these are
        // intentionally the same" context this annotation exists to give.
        // Display-only -- previewValues (the actual submitted/written
        // value) is unaffected either way.
        const sameAsServer = !values.public_url || values.public_url === values.server_url;
        display[field] = sameAsServer ? "(same as server URL)" : values.public_url;
        previewValues[field] = values.public_url || values.server_url || "";
      } else if (field === "sync_max_rate_adjustment") {
        const n = parseFloat(values.sync_max_rate_adjustment);
        display[field] = isNaN(n) ? "" : n.toFixed(2);
        previewValues[field] = isNaN(n) ? 0 : n;
      } else {
        display[field] = values[field] || "";
        previewValues[field] = values[field] || "";
      }
    }

    const groups = filterReviewGroups(display, state.adv);
    reviewEl.innerHTML = "";
    for (const group of groups) {
      const heading = doc.createElement("h3");
      heading.className = "wizard-review-group-title";
      heading.textContent = group.title.toUpperCase();
      reviewEl.appendChild(heading);
      const dl = doc.createElement("dl");
      dl.className = "wizard-review-dl";
      for (const row of group.rows) {
        const dt = doc.createElement("dt");
        dt.textContent = row.label;
        if (row.advanced) dt.classList.add("is-advanced-label");
        const dd = doc.createElement("dd");
        dd.textContent = row.value;
        dl.appendChild(dt);
        dl.appendChild(dd);
      }
      reviewEl.appendChild(dl);
    }

    if (previewSection) previewSection.hidden = !state.adv;
    if (previewEl) previewEl.textContent = state.adv ? buildConfigPreview(previewValues) : "";
  }

  // Open on whichever step needs attention rather than always step 1 --
  // see firstStepWithMissingField's doc comment for why this matters:
  // browser_origins/server_url have no sensible default, so a fresh load
  // can already have required fields missing, and the gate that gets the
  // operator TO those fields is the same one that would otherwise trap
  // them on a step that doesn't contain any of them.
  const initialMissing = requiredFieldsMissing(fieldValues());
  state.current = firstStepWithMissingField(initialMissing);
  state.maxSeen = state.current;
  render();
}

// Normalizes a resolved (count, explicit-unit-or-null) pair into the
// field's actual valid number line -- Group A has no 0 (so 0 or below
// always becomes -1, matching the "typing 0 is an unambiguous disable"
// rule), Group B floors at -1 (or 0 with no negative sentinel) -- the same
// shape clampCount already enforces for stepping, applied here to a typed
// value instead of a step. Shared by resolveDurationBlur below and by
// wireDurationField's live negative-clamp handler, so there is exactly one
// place this normalization rule lives.
function finalizeTypedCount(n, explicitUnit, prevUnit, config) {
  if (config.zeroSentinel === null) {
    if (n <= 0) n = -1;
  } else if (config.negativeSentinel) {
    if (n < 0) n = -1;
  } else if (n < 0) {
    n = 0;
  }
  const sentinel = sentinelUnitFor(n, config);
  return { count: n, unit: explicitUnit || sentinel || restoreUnitOnRaise(prevUnit, config) };
}

// Resolves a duration spin box's raw typed text (on blur) into a
// normalized {count, unit}, reusing duration-control.js's pure functions
// rather than re-deriving the sentinel/clamp rules here: a recognized
// typed suffix (e.g. "5d") wins outright; otherwise typedNegativeState
// handles a leading minus the same way the live per-keystroke handler in
// wireDurationField does (a lone pending "-" left at blur without ever
// being followed by a digit still resolves to the disable sentinel here,
// matching the design source's own blur-time handling for that case); any
// other value is treated as a plain digit string (empty -> 0). By the time
// blur actually runs, live typing (see wireDurationField) will already
// have committed a real negative value to -1, so this negative branch is
// primarily a defensive fallback for the lone-pending-minus case and
// programmatic value changes that never fired an input event.
function resolveDurationBlur(raw, prevUnit, config) {
  const suffix = parseTypedSuffix(raw, config);
  if (suffix) {
    return finalizeTypedCount(suffix.count, suffix.unit, prevUnit, config);
  }
  const neg = typedNegativeState(raw, config);
  if (neg.applies) {
    return finalizeTypedCount(-1, config.negativeSentinel, prevUnit, config);
  }
  const trimmed = raw.trim();
  const digits = trimmed.replace(/[^0-9]/g, "");
  const n = digits === "" ? 0 : Math.min(9999, parseInt(digits, 10));
  return finalizeTypedCount(n, null, prevUnit, config);
}

// Wires one duration field's spin box (count input + unit select + up/down
// arrow buttons) to a hidden `name="<field>"` input holding the actual
// submitted Go-duration string, kept in sync on every change. Press-and-
// hold timing matches the design source exactly (see ARCHITECTURE.md's
// setup wizard section): one immediate step on pointer-down, then a 400ms
// delay before repeating every 55ms, accelerating ×5 past 15 ticks and ×10
// past 40; released on pointer-up/leave or a window-level mouseup/touchend
// so letting go outside the button still stops it.
function wireDurationField(doc, form, field, config, onChange) {
  const hidden = form.elements.namedItem(field);
  const countEl = doc.getElementById(`${field}__count`);
  const unitEl = doc.getElementById(`${field}__unit`);
  const upEl = doc.getElementById(`${field}__up`);
  const downEl = doc.getElementById(`${field}__down`);
  if (!hidden || !countEl || !unitEl) return;

  const SENTINEL_LABELS = { never: "Never", none: "No Time", forever: "Forever" };
  unitEl.innerHTML = "";
  for (const u of config.units) {
    const opt = doc.createElement("option");
    opt.value = u;
    unitEl.appendChild(opt);
  }
  for (const u of [config.zeroSentinel, config.negativeSentinel]) {
    if (!u) continue;
    const opt = doc.createElement("option");
    opt.value = u;
    opt.textContent = SENTINEL_LABELS[u] || u;
    unitEl.appendChild(opt);
  }

  let count, unit;
  ({ count, unit } = parseGoDurationString(hidden.value, config));

  function paint() {
    countEl.value = String(count);
    unitEl.value = unit;
    const labelOpt = unitEl.querySelector(`option[value="${unit}"]`);
    if (labelOpt && config.units.includes(unit)) {
      labelOpt.textContent = pluralizeUnit(unit, count);
    }
    hidden.value = durationToGoString(count, unit, config);
  }
  paint();

  function apply(nextCount, nextUnit) {
    count = nextCount;
    unit = nextUnit;
    paint();
    onChange();
  }

  function step(delta) {
    const nextCount = clampCount(count, delta, config);
    const sentinel = sentinelUnitFor(nextCount, config);
    apply(nextCount, sentinel || restoreUnitOnRaise(unit, config));
  }

  // Live, per-keystroke negative clamp -- verified directly against the
  // design source's own onNumField (not just its README) that typing a
  // negative value commits to the disable sentinel immediately, on every
  // keystroke, not only on blur. A lone leading minus with nothing (or
  // just "0") after it yet is left showing as a bare "-" so the operator
  // can keep typing; any other digit after the minus commits right away.
  // Normal (non-negative) typing is untouched here -- typedNegativeState
  // only ever applies when the raw text has a leading minus.
  countEl.addEventListener("input", () => {
    const neg = typedNegativeState(countEl.value, config);
    if (!neg.applies) return;
    if (neg.pending) {
      countEl.value = "-";
      return;
    }
    apply(neg.count, neg.unit);
  });
  countEl.addEventListener("blur", () => {
    const resolved = resolveDurationBlur(countEl.value, unit, config);
    apply(resolved.count, resolved.unit);
  });
  countEl.addEventListener("keydown", (e) => {
    if (e.key === "ArrowUp") {
      e.preventDefault();
      step(1);
    } else if (e.key === "ArrowDown") {
      e.preventDefault();
      step(-1);
    }
  });
  unitEl.addEventListener("change", () => {
    unit = unitEl.value;
    onChange();
  });

  let holdTimer = null;
  let holdInterval = null;
  function stopHold() {
    clearTimeout(holdTimer);
    clearInterval(holdInterval);
    holdTimer = null;
    holdInterval = null;
  }
  function startHold(dir) {
    stopHold();
    step(dir);
    let elapsed = 0;
    holdTimer = setTimeout(() => {
      holdInterval = setInterval(() => {
        elapsed += 1;
        const magnitude = elapsed > 40 ? 10 : elapsed > 15 ? 5 : 1;
        step(dir * magnitude);
      }, 55);
    }, 400);
  }
  for (const [el, dir] of [
    [upEl, 1],
    [downEl, -1],
  ]) {
    if (!el) continue;
    el.addEventListener("mousedown", (e) => {
      if (e.button !== 0) return;
      e.preventDefault();
      startHold(dir);
    });
    el.addEventListener("touchstart", (e) => {
      e.preventDefault();
      startHold(dir);
    });
    el.addEventListener("mouseup", stopHold);
    el.addEventListener("mouseleave", stopHold);
    el.addEventListener("touchend", stopHold);
  }
  doc.defaultView?.addEventListener("mouseup", stopHold);
  doc.defaultView?.addEventListener("touchend", stopHold);
}

// Step 1's "Show More Info..." / "Hide More Info" disclosure panel --
// collapsed by default, toggled independently of everything else on the
// page.
function wireDisclosure(doc) {
  const panel = doc.getElementById("wizard-disclosure");
  const toggle = doc.getElementById("wizard-disclosure-toggle");
  const body = doc.getElementById("wizard-disclosure-body");
  const label = doc.getElementById("wizard-disclosure-label");
  if (!panel || !toggle || !body || !label) return;
  toggle.addEventListener("click", () => {
    const open = panel.dataset.open === "true";
    panel.dataset.open = String(!open);
    toggle.setAttribute("aria-expanded", String(!open));
    body.hidden = open;
    label.textContent = open ? "Show More Info..." : "Hide More Info";
  });
}

// Listen Address is a split IP + Port pair in the UI, reassembled into the
// single hidden `name="listen_address"` field the server actually expects
// (config.ValidateListenAddress / net.SplitHostPort) -- see
// ARCHITECTURE.md's setup wizard section for why the two visible inputs
// stay unnamed rather than submitted directly.
function wireListenAddress(doc, form, onChange) {
  const hidden = form.elements.namedItem("listen_address");
  const ipEl = doc.getElementById("listen_ip");
  const portEl = doc.getElementById("listen_port");
  if (!hidden || !ipEl || !portEl) return;

  // Seed the two visible fields from whatever the hidden field already
  // holds (the server's default, or a preserved value on an error
  // re-render) -- net.SplitHostPort allows an empty host (":8080"), which
  // splits to an empty ip here too; the ip field's own blur handler fills
  // in 0.0.0.0 the first time it's touched, matching the design source.
  const lastColon = String(hidden.value || "").lastIndexOf(":");
  if (lastColon !== -1) {
    // hidden.value's default is the bare ":8080" (config.ResolveListenAddress's
    // own default -- no host, meaning "all interfaces" to net.SplitHostPort),
    // which splits to an empty ip substring here. Falling back to "0.0.0.0"
    // matches what reassembleListenAddress already does for the hidden field
    // on every sync() below -- without it, the visible ip box would ship
    // empty (placeholder only) instead of the design source's real prefilled
    // "0.0.0.0" default.
    ipEl.value = hidden.value.slice(0, lastColon) || "0.0.0.0";
    portEl.value = hidden.value.slice(lastColon + 1);
  }

  function sync() {
    hidden.value = reassembleListenAddress(ipEl.value, portEl.value);
    onChange();
  }
  ipEl.addEventListener("blur", () => {
    if (ipEl.value.trim() === "") ipEl.value = "0.0.0.0";
    sync();
  });
  portEl.addEventListener("input", () => {
    portEl.value = portEl.value.replace(/[^0-9]/g, "");
  });
  portEl.addEventListener("blur", () => {
    portEl.value = String(clampPort(portEl.value));
    sync();
  });
  ipEl.addEventListener("input", sync);
  sync();
}

// Max Playback Rate Adjustment: a single 0.00-1.00 spin box, step 0.01, no
// unit dropdown -- reuses the same press-and-hold arrow mechanics as the
// duration fields (wireDurationField) but with its own two-decimal
// clamping instead of duration-control.js's integer count model.
function wireRateField(doc, form, onChange) {
  const el = form.elements.namedItem("sync_max_rate_adjustment");
  const upEl = doc.getElementById("sync_max_rate_adjustment__up");
  const downEl = doc.getElementById("sync_max_rate_adjustment__down");
  if (!el) return;

  function clamp(v) {
    const n = parseFloat(v);
    return Math.min(1, Math.max(0, isNaN(n) ? 0 : n));
  }
  function set(v) {
    el.value = clamp(v).toFixed(2);
    onChange();
  }
  el.addEventListener("blur", () => set(el.value));

  let holdTimer = null;
  let holdInterval = null;
  function stopHold() {
    clearTimeout(holdTimer);
    clearInterval(holdInterval);
  }
  function bump(dir) {
    set((parseFloat(el.value) || 0) + dir * 0.01);
  }
  function startHold(dir) {
    stopHold();
    bump(dir);
    holdTimer = setTimeout(() => {
      holdInterval = setInterval(() => bump(dir), 55);
    }, 400);
  }
  for (const [btn, dir] of [
    [upEl, 1],
    [downEl, -1],
  ]) {
    if (!btn) continue;
    btn.addEventListener("mousedown", (e) => {
      if (e.button !== 0) return;
      e.preventDefault();
      startHold(dir);
    });
    btn.addEventListener("mouseup", stopHold);
    btn.addEventListener("mouseleave", stopHold);
  }
  doc.defaultView?.addEventListener("mouseup", stopHold);
}
