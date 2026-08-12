package httpapi

import (
	"html/template"
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

// registerPages attaches the server-rendered HTML shell and static asset
// routes. The pages themselves hold almost no server logic — they're a
// thin shell that vanilla JS (web/static/js/*.js) fills in by calling the
// JSON API, per the spec's "server's authoritative state is the single
// source of truth, client is a thin renderer" design.
func registerPages(mux *http.ServeMux) {
	staticSub, err := webassets.StaticFS()
	if err != nil {
		panic("webassets: static fs: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /static/", http.StripPrefix("/static/", fileServer))

	mux.HandleFunc("GET /party/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		pageTemplates.ExecuteTemplate(w, "party.html", map[string]string{"PartyID": r.PathValue("id")})
	})

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		pageTemplates.ExecuteTemplate(w, "index.html", nil)
	})
}
