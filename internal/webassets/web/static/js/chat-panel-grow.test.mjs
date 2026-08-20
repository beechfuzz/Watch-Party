// Run with: node --test internal/webassets/web/static/js/chat-panel-grow.test.mjs
//
// What this test does and doesn't prove: it's a static text check that the
// flex-grow rule is still present in style.css, nothing more -- a tripwire
// against someone deleting or weakening the fix, not proof that a browser
// actually renders Chat filling the column. This project has no jsdom (see
// CLAUDE.md/ARCHITECTURE.md §1.6, §10.2's "no jsdom in this project"), and
// jsdom doesn't implement flexbox layout at all, so it couldn't catch this
// class of bug even if added. The actual regression -- none of the three
// sidebar blocks had flex-grow, so leftover .party-sidebar-col height went
// unclaimed below the last block (Chat) -- was verified empirically against
// a real Chromium instance, not inferred -- see ARCHITECTURE.md §11.6. A
// real regression test for the rendered behavior itself would need
// Playwright/a real browser engine, which is a separate infrastructure
// decision (this repo has no package.json/node_modules at all today) -- not
// something this test takes on.
import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const cssPath = fileURLToPath(new URL("../css/style.css", import.meta.url));
const css = readFileSync(cssPath, "utf8");

test("style.css gives .chat-block flex-grow so it fills the sidebar column's leftover height", () => {
  // Whitespace-tolerant, not tied to formatting -- just needs a rule
  // targeting .chat-block (combined with .sidebar-block, since a bare
  // .chat-block rule would be outranked by the later, equal-specificity
  // .sidebar-block { flex-grow: 0; } rule under normal CSS cascade order)
  // that sets flex-grow to a positive value.
  const chatGrowRule = /\.chat-block\.sidebar-block\s*\{[^}]*flex-grow\s*:\s*[1-9][^}]*\}/;
  assert.match(
    css,
    chatGrowRule,
    "style.css must contain `.chat-block.sidebar-block { flex-grow: 1; }` (or equivalent) -- " +
      "without it, Chat (the last of the three sidebar blocks) doesn't claim leftover " +
      ".party-sidebar-col height, leaving a visible gap below it while Watching Now and " +
      "Playlist, sized only to their own flex-basis, render correctly. See ARCHITECTURE.md §11.6."
  );
});
