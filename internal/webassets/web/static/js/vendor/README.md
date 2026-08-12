# Vendored third-party scripts

Watch Party has no frontend build pipeline (see ARCHITECTURE.md §1.6), so
third-party scripts are vendored as pre-built files rather than pulled in
via a bundler or npm at build time. This also means the app doesn't depend
on a CDN being reachable at runtime — everything it serves, it ships.

## hls.js

- **File:** `hls.min.js`
- **Version:** 1.7.0
- **Source:** https://registry.npmjs.org/hls.js/-/hls.js-1.7.0.tgz (`dist/hls.min.js`)
- **License:** Apache-2.0 (`hls.js.LICENSE`)
- **Why:** only Safari supports HLS (`.m3u8`) natively in a `<video>`
  element. Emby's transcoded-playback path always produces an HLS stream,
  so Chrome/Firefox/Edge need this to play transcoded content at all — see
  `player.js` and ARCHITECTURE.md §5.

To upgrade: download a newer `dist/hls.min.js` from the same package and
replace this file; there's nothing else to regenerate.
