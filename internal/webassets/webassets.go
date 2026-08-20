// Package webassets embeds Watch Party's server-rendered HTML templates and
// static JS/CSS into the binary, so the final container image is just the
// single static Go binary the spec calls for — no separate asset directory
// to copy or mount.
package webassets

import (
	"embed"
	"html/template"
	"io/fs"
)

//go:embed web/static
var staticFS embed.FS

// The all: prefix is required: without it, //go:embed silently excludes
// any file whose name starts with "_" or "." (Go's embed package
// convention for "hidden" files) -- which would drop _sidebar.html from
// this FS entirely, with no error at build or parse time. See
// ARCHITECTURE.md §11.4.
//
//go:embed all:web/templates
var templatesFS embed.FS

// StaticFS returns the embedded static assets rooted at "static/..." so
// callers can serve it directly under an HTTP prefix.
func StaticFS() (fs.FS, error) {
	return fs.Sub(staticFS, "web/static")
}

// Templates parses every embedded *.html template. html/template
// auto-escapes all dynamic values, which matters here since page content
// includes user-controlled Emby display names.
func Templates() (*template.Template, error) {
	return template.ParseFS(templatesFS, "web/templates/*.html")
}
