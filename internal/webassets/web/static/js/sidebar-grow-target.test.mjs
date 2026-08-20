// Run with: node --test internal/webassets/web/static/js/sidebar-grow-target.test.mjs
//
// What this test does and doesn't prove: it's a static text check that the
// generic .is-grow-target CSS rule is still present in style.css, nothing
// more -- a tripwire against someone deleting or weakening it, not proof
// that a browser actually renders the last-expanded panel filling the
// column. This project has no jsdom (see CLAUDE.md/ARCHITECTURE.md §1.6,
// §10.2's "no jsdom in this project"), and jsdom doesn't implement flexbox
// layout at all, so it couldn't catch this class of bug even if added. The
// underlying regression this generalizes -- none of the three sidebar
// blocks had flex-grow, so leftover .party-sidebar-col height went
// unclaimed below the last block -- was originally verified empirically
// against a real Chromium instance for the Chat-only case (ARCHITECTURE.md
// §11.6), then found to reopen identically under Playlist once Chat itself
// was collapsed -- which is what this generalization (§11.7) fixes.
// sidebar-panels.test.mjs's lastExpandedKey tests cover the pure
// "which panel should grow" logic; this file only pins that the CSS rule
// applying that decision is still wired up.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const cssPath = fileURLToPath(new URL("../css/style.css", import.meta.url));
const css = readFileSync(cssPath, "utf8");

test("style.css gives .is-grow-target flex-grow so the last-expanded panel fills the sidebar column's leftover height", () => {
  // Whitespace-tolerant, not tied to formatting -- just needs a rule
  // targeting .sidebar-block.is-grow-target (combined, since a bare
  // .is-grow-target rule would be outranked by the equal-specificity
  // .sidebar-block { flex-grow: 0; } rule under normal CSS cascade order)
  // that sets flex-grow to a positive value.
  const growTargetRule = /\.sidebar-block\.is-grow-target\s*\{[^}]*flex-grow\s*:\s*[1-9][^}]*\}/;
  assert.match(
    css,
    growTargetRule,
    "style.css must contain `.sidebar-block.is-grow-target { flex-grow: 1; }` (or equivalent) -- " +
      "without it, whichever panel is last-expanded doesn't claim leftover .party-sidebar-col " +
      "height, reopening the gap ARCHITECTURE.md §11.6/§11.7 fixed. player.js's applyPanelState " +
      "is what toggles this class onto the panel sidebar-panels.js's lastExpandedKey names."
  );
});

test("player.js applies is-grow-target based on lastExpandedKey, not a hardcoded panel", () => {
  const jsPath = fileURLToPath(new URL("./player.js", import.meta.url));
  const js = readFileSync(jsPath, "utf8");
  assert.match(
    js,
    /lastExpandedKey/,
    "player.js must derive the grow target from sidebar-panels.js's lastExpandedKey -- " +
      "a hardcoded 'chat' (or any other fixed key) reintroduces the exact bug this generalizes."
  );
  assert.match(
    js,
    /classList\.toggle\(\s*["']is-grow-target["']/,
    "player.js must toggle .is-grow-target onto the block, driven by state -- " +
      "a CSS-only rule with no JS toggle can't move with collapse/expand."
  );
});
