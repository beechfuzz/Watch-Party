# Vendored fonts

Same rationale as `js/vendor/README.md`: no frontend build pipeline, so
these are vendored directly rather than linked from Google Fonts' CDN at
runtime — a self-hosted app shouldn't need internet access just to render
its own UI correctly.

Each is a single variable-weight WOFF2 file (Google's own CSS2 API serves
the same file for every weight of a given family via `font-variation-settings`
under the hood — there's genuinely only one file per family, not one per
weight), covering only the `latin` Unicode subset, matching what the UI
actually needs.

## Inter

- **File:** `inter-variable.woff2`
- **Source:** `fonts.gstatic.com` (via `fonts.googleapis.com/css2?family=Inter:wght@400;500;600`), latin subset
- **License:** SIL Open Font License 1.1 (`OFL-Inter.txt`)
- **Used for:** body text (`--font-body`)

## Space Grotesk

- **File:** `space-grotesk-variable.woff2`
- **Source:** `fonts.gstatic.com` (via `fonts.googleapis.com/css2?family=Space+Grotesk:wght@500;600;700`), latin subset
- **License:** SIL Open Font License 1.1 (`OFL-SpaceGrotesk.txt`)
- **Used for:** headings/display text (`--font-display`)

## JetBrains Mono

- **File:** `jetbrains-mono-variable.woff2`
- **Source:** `fonts.gstatic.com` (via `fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500`), latin subset
- **License:** SIL Open Font License 1.1 (`OFL-JetBrainsMono.txt`)
- **Used for:** monospaced text — timecodes, sync latency, invite codes (`--font-mono`)

To upgrade any of these: fetch `https://fonts.googleapis.com/css2?family=<Family>:wght@<weights>` with a
modern browser User-Agent (Google serves WOFF2 only to UAs it recognizes as
supporting it), find the `/* latin */`-commented `@font-face` block(s), and
download the `url(...)` referenced there.
