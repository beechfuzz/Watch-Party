package httpapi

import (
	"bytes"
	"errors"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/beechfuzz/watch-party/internal/session"
	"github.com/beechfuzz/watch-party/internal/webassets"
)

var pageTemplates = mustParseTemplates()

func mustParseTemplates() *template.Template {
	t, err := webassets.Templates()
	if err != nil {
		panic("webassets: parse templates: " + err.Error())
	}
	return t
}

// pageData is the data every server-rendered page template (and the shared
// sidebar partial they both include, _sidebar.html) executes against.
// ActiveNav marks which sidebar nav item, if any, should render as
// "is-active" -- "home" on the dashboard; left empty on the party room,
// since none of the sidebar's destinations represent "you're in a party"
// today. Authenticated gates index.html's home-dashboard markup
// (home-section and create-party-dialog) -- see registerPages.
type pageData struct {
	PartyID       string
	ActiveNav     string
	Title         string
	Authenticated bool
}

// registerPages attaches the server-rendered HTML shell and static asset
// routes. The pages themselves hold almost no server logic — they're a
// thin shell that vanilla JS (web/static/js/*.js) fills in by calling the
// JSON API, per the spec's "server's authoritative state is the single
// source of truth, client is a thin renderer" design.
//
// Both page routes check session validity before rendering -- see
// ARCHITECTURE.md's home-dashboard-auth-bypass postmortem. Any
// Sessions.Authenticate error, not just "no session", is treated as
// unauthenticated here: a page load failing safe to the login screen (or a
// redirect to it) is preferable to a 500 on the app's front door, and
// unlike the /api/... handlers wrapped in withAuth, there's no JSON error
// body to distinguish "unauthorized" from "internal error" for a page
// response anyway. The real error is still logged.
func registerPages(mux *http.ServeMux, app *App) {
	staticSub, err := webassets.StaticFS()
	if err != nil {
		panic("webassets: static fs: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /static/", http.StripPrefix("/static/", noCache(fileServer)))

	mux.HandleFunc("GET /party/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := app.Sessions.Authenticate(r.Context(), r); err != nil {
			if !errors.Is(err, session.ErrNoSession) && !errors.Is(err, session.ErrSessionExpired) {
				app.Logger.Error("session authenticate failed", "error", err)
			}
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		renderPage(w, app.Logger, pageTemplates, "party.html", pageData{PartyID: r.PathValue("id"), Title: app.Title})
	})

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, err := app.Sessions.Authenticate(r.Context(), r)
		authenticated := err == nil
		if err != nil && !errors.Is(err, session.ErrNoSession) && !errors.Is(err, session.ErrSessionExpired) {
			app.Logger.Error("session authenticate failed", "error", err)
		}
		renderPage(w, app.Logger, pageTemplates, "index.html", pageData{ActiveNav: "home", Title: app.Title, Authenticated: authenticated})
	})
}

// renderPage executes the named template into a buffer before writing
// anything to w, so a template execution failure -- a bad {{template}}
// reference, a partial missing from the embedded FS, anything -- can still
// produce a real 500 instead of a 200 with a truncated or empty body. The
// full error (which may name internal template/file details) is logged;
// the client only ever sees a generic message. See ARCHITECTURE.md §11.4.
//
// data is any rather than pageData specifically so the setup wizard
// (setup_wizard.go, its own setupPageData shape -- unrelated fields to
// pageData's PartyID/ActiveNav/Title) can reuse this same helper instead
// of a second template-execution code path; html/template.ExecuteTemplate
// itself already takes an untyped data argument, so this is not a loss of
// safety, just passing that through.
func renderPage(w http.ResponseWriter, logger *slog.Logger, tmpl *template.Template, name string, data any) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		logger.Error("render page template", "template", name, "error", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(buf.Bytes())
}

// noCache forces every response through it to be revalidated before use --
// by a browser, or by an intermediate cache/CDN a deployment happens to sit
// behind -- rather than served straight from a stale copy. This app has no
// frontend build pipeline (§1.6): static assets are served at fixed URLs
// with no content hash in the filename, so a new deploy overwrites the
// same URL's content in place. Go's http.FileServer over an embed.FS sends
// no cache-lifetime header of its own, which is exactly the condition under
// which some CDNs (e.g. Cloudflare's default cache level) apply their own
// heuristic caching by file extension regardless of what the origin did or
// didn't send — silently serving a pre-deploy JS/CSS file indefinitely. See
// ARCHITECTURE.md §1.7.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
