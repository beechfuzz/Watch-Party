package httpapi

import (
	"bytes"
	"html/template"
	"io"
	"log/slog"
	"net/http"
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
// instead of silently passing. Authenticated: true, since the sidebar
// partial only renders inside index.html's home-section, which is now
// gated on it (see the home-dashboard-auth-bypass postmortem).
func TestRenderPage_Success_ReturnsContent(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	rec := httptest.NewRecorder()
	renderPage(rec, logger, pageTemplates, "index.html", pageData{ActiveNav: "home", Title: "Watch Party", Authenticated: true})

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

// TestIndexPage_Unauthenticated_ServesLoginOnly pins the fix for the
// unauthenticated home-dashboard exposure (ARCHITECTURE.md's
// home-dashboard-auth-bypass postmortem): without a session cookie, GET /
// must serve the login markup only -- home-section and
// create-party-dialog must not appear anywhere in the body, not merely be
// hidden by an attribute a client can strip.
func TestIndexPage_Unauthenticated_ServesLoginOnly(t *testing.T) {
	_, srv := newTestApp(t)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	if !strings.Contains(got, `id="login-section"`) || !strings.Contains(got, `id="login-form"`) {
		t.Errorf("response body missing login markup; got: %q", got)
	}
	for _, marker := range []string{`id="home-section"`, `id="create-party-btn"`, `id="create-party-dialog"`} {
		if strings.Contains(got, marker) {
			t.Errorf("unauthenticated response body must not contain %q, but it does: %q", marker, got)
		}
	}
}

// TestIndexPage_Authenticated_ServesFullShell confirms today's behavior is
// unchanged for a valid session: both login-section and home-section still
// render, exactly as before the fix.
func TestIndexPage_Authenticated_ServesFullShell(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)

	resp, err := c.http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)

	for _, marker := range []string{`id="login-section"`, `id="home-section"`, `id="create-party-btn"`, `id="create-party-dialog"`} {
		if !strings.Contains(got, marker) {
			t.Errorf("authenticated response body missing %q; got: %q", marker, got)
		}
	}
}

// TestPartyPage_Unauthenticated_Redirects pins the fix for GET
// /party/{id}: without a session cookie, it must redirect to / instead of
// rendering the party shell for an arbitrary/unauthenticated caller.
func TestPartyPage_Unauthenticated_Redirects(t *testing.T) {
	_, srv := newTestApp(t)
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}

	resp, err := client.Get(srv.URL + "/party/some-id")
	if err != nil {
		t.Fatalf("GET /party/some-id: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), `id="party-title"`) {
		t.Errorf("redirect response body must not contain party shell markup; got: %q", body)
	}
}

// TestPartyPage_Authenticated_ServesShell confirms today's behavior is
// unchanged for a valid session: the party shell still renders normally.
func TestPartyPage_Authenticated_ServesShell(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)

	resp, err := c.http.Get(srv.URL + "/party/some-id")
	if err != nil {
		t.Fatalf("GET /party/some-id: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `id="party-title"`) {
		t.Errorf("authenticated response body missing party shell markup; got: %q", body)
	}
}
