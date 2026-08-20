// Run with: node --test internal/webassets/web/static/js/hidden-guard.test.mjs
//
// What this test does and doesn't prove: it's a static text check that the
// guarding CSS rule is still present in style.css, nothing more -- a
// tripwire against someone deleting or weakening the fix, not proof that a
// browser actually renders these elements as hidden. This project has no
// jsdom (see CLAUDE.md/ARCHITECTURE.md §1.6, §10.2's "no jsdom in this
// project"), and jsdom's getComputedStyle wouldn't catch this class of bug
// even if added -- it only reflects an element's own inline `style`
// attribute, not cascade resolution against a real stylesheet. The actual
// regression (a `.btn`/`.icon-btn`/`.host-controls-row` class's own
// `display` value silently outranking the browser's default
// `[hidden] { display: none }` UA rule, because author-origin CSS always
// wins over user-agent-origin CSS regardless of specificity) was verified
// empirically against a real Chromium instance, not inferred -- see
// ARCHITECTURE.md §11.5. A real regression test for the rendered behavior
// itself would need Playwright/a real browser engine, which is a separate
// infrastructure decision (this repo has no package.json/node_modules at
// all today) -- not something this test takes on.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const cssPath = fileURLToPath(new URL("../css/style.css", import.meta.url));
const css = readFileSync(cssPath, "utf8");

test("style.css re-asserts [hidden] with !important, so it beats any class's own display value", () => {
  // Whitespace-tolerant, not tied to formatting -- just needs a `[hidden]`
  // selector rule whose declaration block sets `display: none` and is
  // `!important`, so it wins over any author-origin class rule (.btn,
  // .icon-btn, .host-controls-row, or any future one) regardless of
  // selector specificity or source order.
  const hiddenRule = /\[hidden\]\s*\{[^}]*display\s*:\s*none\s*!important[^}]*\}/;
  assert.match(
    css,
    hiddenRule,
    "style.css must contain `[hidden] { display: none !important; }` (or equivalent) -- " +
      "without it, any class that sets its own `display` (e.g. .btn, .icon-btn, " +
      ".host-controls-row) silently overrides the browser's default hidden-attribute " +
      "behavior, since author CSS always beats the user-agent stylesheet regardless of " +
      "specificity. See ARCHITECTURE.md §11.5."
  );
});
