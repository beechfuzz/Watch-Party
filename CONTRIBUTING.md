# Contributing — Issue Workflow

This defines how work enters this repo. It exists to make sure every non-trivial change traces back to a written Problem/Goal/Scope before implementation starts, and complements — not duplicates — the process already defined in `CLAUDE.md` and the plan-approval gate.

## Core rule

**Every behavior-changing PR must reference an Issue.** The Issue is the unit of intent; the PR is the unit of change. `Fixes #N` / `Closes #N` in the PR body is what makes that traceable — an Issue with no linked PR, or a PR with no linked Issue, breaks the chain.

## What requires an Issue

Requires one:
- New features or behavior changes
- Bug fixes
- Refactors
- Anything Claude Code will implement

Does not require one (`trivial` label on the PR instead):
- Typo/formatting/comment-only changes
- Dependency bumps with no behavior change
- Any one-line change with zero scope ambiguity

If you're unsure which bucket something falls in, it needs an Issue — the cost of an unnecessary Issue is low; the cost of an unscoped Claude Code session is not.

## Issue template

```markdown
## Problem
What's wrong or what opportunity exists?

## Goal
What should be different when this is done?

## Scope
What is included in this change?

## Non-Goals
What is explicitly NOT included, even if related?

## Acceptance Criteria
- [ ] Verifiable, testable condition
- [ ] ...

## Notes
Relevant context, links, prior discussion.
```

**Non-Goals is the highest-value section.** It's the written boundary Claude Code's plan gets checked against before you approve it — the same boundary "investigate-first / plan-first" already enforces, just set one step earlier and made durable instead of living only in chat.

**Acceptance Criteria doubles as the close-out checklist** — the same list your pasted terminal output and CI status are checked against at close-out.

## Labels

Two axes plus two flags. No priority labels, no status labels — priority is whatever you choose to work on next, and status is tracked by the Issue being open/closed plus its linked PR state. No `type:` prefix — GitHub's native Issue Type field would be the more current mechanism for this axis, but it's currently organization-only and unavailable on this personal-account repo; plain labels are used instead, and there's no `area` label whose name collides with a type name, so no namespace prefix is needed here.

### Type (apply exactly one)

- `bug` — existing behavior doesn't match documented or intended behavior. Includes a missing or broken security control — there is no dedicated security type, so a security gap is classified as a `bug`.
- `feature` — adds new behavior or capability that didn't exist before, even if narrowly scoped.
- `refactor` — changes internal structure or implementation with no intended behavior change visible to a user.

**Tie-breaker:** if a change fixes broken behavior and also introduces new behavior as a side effect, classify by the problem that triggered the Issue, not every effect of the eventual fix. A login-CSRF fix that happens to also restructure the auth handler is still `bug`, not `refactor`.

### Area (apply one or more)

- `area:sync` — playback synchronization, drift correction, party-state authority, WebSocket play/pause/seek handling.
- `area:chat` — real-time text chat panel, message storage, rate limiting.
- `area:playlist` — playlist mutation, ordering, auto-advance, and party-settings tied to the playback queue.
- `area:wizard` — the setup wizard flow and the `config.jsonc` layer, including wizard-specific UI. Use this instead of `area:ui` for anything inside the wizard.
- `area:ui` — frontend layout and presentation not owned by a more specific area (sidebar, resizing, styling, general page layout). Use the more specific area label when one applies; `area:ui` is the fallback for frontend concerns, not the default.
- `area:infra` — deployment, container, reverse-proxy, CI/CD.
- `area:auth` — login, session, CSRF/Origin, token handling. Distinct from `area:infra`.
- `area:voice` — the planned Discord-style SFU voice chat work. Distinct from `area:chat` (text chat). Added ahead of that work moving from exploration into real Issues.

### Flags

- `trivial` — exempts a PR from needing a linked Issue.
- `security` — cross-cutting flag for any Issue with a security implication, applied alongside its type and area label(s), not in place of them.

### If no label is an accurate fit

Flag the gap explicitly in your report rather than applying the closest available label to force a fit. A flagged gap is a cheap, correctable miss; a silently forced label is a miscategorization that looks resolved and isn't.

## Sub-issues

Use GitHub sub-issues only when a body of work genuinely decomposes into independently mergeable pieces — e.g. the voice-chat SFU work (transport, moderation controls, audio verification strategy) once it leaves exploration. Don't create sub-issues for implementation steps within a single PR.

## Workflow

1. Open an Issue using the template above before starting any Claude Code session for that change.
2. The Issue's Scope/Non-Goals is what gets pasted into or referenced by the Claude Code prompt.
3. Claude Code's plan is checked against the Issue's Scope/Non-Goals before you give the literal "approved."
4. Close-out verification (pasted terminal output, live CI check) is checked against the Issue's Acceptance Criteria, not just "looks done."
5. The merging PR includes `Fixes #N` / `Closes #N` so the Issue closes automatically and the history stays queryable.

## Deliberately excluded

- **Milestones / GitHub Projects** — not adopted. `overview.md`'s "current state" / "on the horizon" sections already serve that function for a single-maintainer repo; a board would duplicate it with no second reader to justify the upkeep.
- **Priority and status labels** — not adopted, for the same reason: nothing consumes them but you, and you already know what's next.
- **YAML issue forms** — not adopted; a pasted Markdown template gives the same structure with less repo overhead.

## Future (not yet implemented)

A GitHub Action that fails a PR check when the PR body has no `#`-issue reference and no `trivial` label, so the rule is self-enforcing rather than memory-dependent. Deferred until the manual version has run long enough to prove out the template and label set.
