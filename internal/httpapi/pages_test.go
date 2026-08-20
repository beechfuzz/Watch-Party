package httpapi

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRenderPage_TemplateExecutionFailure_Returns500 pins the fix for the
// blank-page bug (ARCHITECTURE.md §11.4): a template execution failure --
// here, a template that references an undefined partial, the same shape of
// failure that "no such template \"sidebar\"" was -- must produce a real
// 500 with a generic body, not a 200 with a truncated/empty one. The real
// error must still reach the log, just never the client.
func TestRenderPage_TemplateExecutionFailure_Returns500(t *testing.T) {
	broken := template.Must(template.New("index.html").Parse(`{{template "missing-partial" .}}`))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	rec := httptest.NewRecorder()
	renderPage(rec, logger, broken, "index.html", pageData{ActiveNav: "home"})

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "missing-partial") || strings.Contains(body, "no such template") {
		t.Errorf("response body leaked template internals: %q", body)
	}
	if strings.TrimSpace(body) == "" {
		t.Error("response body is empty, want a generic error message")
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "missing-partial") {
		t.Errorf("log output = %q, want it to contain the real template error", logged)
	}
}

// TestRenderPage_Success_ReturnsContent exercises renderPage against the
// real, production embedded template set -- including _sidebar.html --
// so a regression in the //go:embed all: prefix (ARCHITECTURE.md §11.4)
// fails this test with the real "no such template \"sidebar\"" error
// instead of silently passing.
func TestRenderPage_Success_ReturnsContent(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	rec := httptest.NewRecorder()
	renderPage(rec, logger, pageTemplates, "index.html", pageData{ActiveNav: "home"})

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (log: %s)", rec.Code, logBuf.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `brand-name">Watch Party`) {
		t.Errorf("response body missing expected sidebar markup; got: %q", body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}
