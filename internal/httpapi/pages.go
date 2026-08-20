package httpapi

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http"

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
// today.
type pageData struct {
	PartyID   string
	ActiveNav string
}

// registerPages attaches the server-rendered HTML shell and static asset
// routes. The pages themselves hold almost no server logic — they're a
// thin shell that vanilla JS (web/static/js/*.js) fills in by calling the
// JSON API, per the spec's "server's authoritative state is the single
// source of truth, client is a thin renderer" design.
func registerPages(mux *http.ServeMux, logger *slog.Logger) {
	staticSub, err := webassets.StaticFS()
	if err != nil {
		panic("webassets: static fs: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /static/", http.StripPrefix("/static/", noCache(fileServer)))

	mux.HandleFunc("GET /party/{id}", func(w http.ResponseWriter, r *http.Request) {
		renderPage(w, logger, pageTemplates, "party.html", pageData{PartyID: r.PathValue("id")})
	})

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		renderPage(w, logger, pageTemplates, "index.html", pageData{ActiveNav: "home"})
	})
}

// renderPage executes the named template into a buffer before writing
// anything to w, so a template execution failure -- a bad {{template}}
// reference, a partial missing from the embedded FS, anything -- can still
// produce a real 500 instead of a 200 with a truncated or empty body. The
// full error (which may name internal template/file details) is logged;
// the client only ever sees a generic message. See ARCHITECTURE.md §11.4.
func renderPage(w http.ResponseWriter, logger *slog.Logger, tmpl *template.Template, name string, data pageData) {
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
