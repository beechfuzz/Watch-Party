package httpapi

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	csrfTokenRe    = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
	generatedKeyRe = regexp.MustCompile(`<code>([^<]+)</code>`)
)

// newSetupWizardServer starts a real httptest.Server over a fresh mux with
// the wizard registered against a fresh temp configPath, returning the
// server, the configPath, and the configWritten channel a successful POST
// signals on. Callers must Close() the server.
func newSetupWizardServer(t *testing.T) (*httptest.Server, string, <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.jsonc")
	configWritten := make(chan struct{}, 1)

	mux := http.NewServeMux()
	if err := RegisterSetupWizardRoutes(mux, slog.Default(), configPath, configWritten); err != nil {
		t.Fatalf("RegisterSetupWizardRoutes: %v", err)
	}
	return httptest.NewServer(mux), configPath, configWritten
}

func newJarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{Jar: jar}
}

func mustGetBody(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func extractCSRFToken(t *testing.T, body string) string {
	t.Helper()
	m := csrfTokenRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("could not find csrf_token in body:\n%s", body)
	}
	return m[1]
}

func extractGeneratedKey(t *testing.T, body string) string {
	t.Helper()
	m := generatedKeyRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("could not find generated key <code> block in body:\n%s", body)
	}
	return m[1]
}

func validWizardForm() url.Values {
	v := url.Values{}
	v.Set("title", "Test Party")
	v.Set("log_level", "info")
	v.Set("browser_origins", "https://watchparty.example.com")
	v.Set("listen_address", ":8080")
	v.Set("session_idle_timeout", "24h")
	v.Set("session_age_timeout", "720h")
	v.Set("host_grace_period", "20s")
	v.Set("inactivity_timeout", "48h")
	v.Set("progress_interval", "10s")
	v.Set("sync_snapshot_interval", "4s")
	v.Set("sync_soft_drift", "300ms")
	v.Set("sync_hard_drift", "1500ms")
	v.Set("sync_max_rate_adjustment", "0.05")
	v.Set("server_url", "https://emby.example.com")
	v.Set("public_url", "")
	return v
}

func TestSetupWizard_Healthz_Returns200(t *testing.T) {
	srv, _, _ := newSetupWizardServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want \"ok\"", body)
	}
}

func TestSetupWizard_GetRoot_RendersFormWithCSRFCookieAndAllFields(t *testing.T) {
	srv, _, _ := newSetupWizardServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	body := string(bodyBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}

	for _, field := range []string{
		"title", "log_level", "browser_origins", "listen_address",
		"session_idle_timeout", "session_age_timeout", "host_grace_period",
		"inactivity_timeout", "progress_interval", "sync_snapshot_interval",
		"sync_soft_drift", "sync_hard_drift", "sync_max_rate_adjustment",
		"server_url", "public_url",
	} {
		if !strings.Contains(body, `name="`+field+`"`) {
			t.Errorf("form is missing field %q", field)
		}
	}

	extractCSRFToken(t, body) // fails the test itself if absent

	// Check the raw Set-Cookie header directly -- net/http/cookiejar's
	// Jar.Cookies() intentionally strips HttpOnly/Secure/SameSite (they're
	// meaningless on an outgoing request, only relevant to how a browser
	// stores/guards the cookie), so this can't be checked through a jar.
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == setupCSRFCookieName {
			found = true
			if !c.HttpOnly {
				t.Error("CSRF cookie is not HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("CSRF cookie SameSite = %v, want Strict", c.SameSite)
			}
		}
	}
	if !found {
		t.Errorf("no %s cookie set on GET /", setupCSRFCookieName)
	}
}

func TestSetupWizard_ConcurrentGETs_IndependentCSRFPairs(t *testing.T) {
	srv, _, _ := newSetupWizardServer(t)
	defer srv.Close()

	client1 := newJarClient(t)
	client2 := newJarClient(t)

	_, body1 := mustGetBody(t, client1, srv.URL+"/")
	_, body2 := mustGetBody(t, client2, srv.URL+"/")

	token1 := extractCSRFToken(t, body1)
	token2 := extractCSRFToken(t, body2)
	if token1 == token2 {
		t.Error("two independent GET / calls got the same CSRF token; each render should mint its own")
	}

	// Each client's own token+cookie pair must still work independently.
	form1 := validWizardForm()
	form1.Set("csrf_token", token1)
	resp1, err := client1.PostForm(srv.URL+"/", form1)
	if err != nil {
		t.Fatalf("client1 POST: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Errorf("client1 POST status = %d, want 200", resp1.StatusCode)
	}
}

func TestSetupWizard_PostWithoutCSRFCookie_Returns403NoFileWritten(t *testing.T) {
	srv, configPath, _ := newSetupWizardServer(t)
	defer srv.Close()

	form := validWizardForm()
	form.Set("csrf_token", "some-token-with-no-matching-cookie")

	resp, err := http.PostForm(srv.URL+"/", form) // no jar -- no cookie sent at all
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("expected no config.jsonc written, stat err = %v", err)
	}
}

func TestSetupWizard_PostWithMismatchedCSRF_Returns403NoFileWritten(t *testing.T) {
	srv, configPath, _ := newSetupWizardServer(t)
	defer srv.Close()

	client := newJarClient(t)
	_, body := mustGetBody(t, client, srv.URL+"/")
	extractCSRFToken(t, body) // real cookie now held by the jar

	form := validWizardForm()
	form.Set("csrf_token", "deliberately-wrong-token")

	resp, err := client.PostForm(srv.URL+"/", form)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("expected no config.jsonc written, stat err = %v", err)
	}
}

func TestSetupWizard_PostInvalidValues_ReRendersWithErrorsNoFileWritten(t *testing.T) {
	srv, configPath, _ := newSetupWizardServer(t)
	defer srv.Close()

	client := newJarClient(t)
	_, getBody := mustGetBody(t, client, srv.URL+"/")
	token := extractCSRFToken(t, getBody)

	form := validWizardForm()
	form.Set("csrf_token", token)
	form.Set("title", "   ")                // blank after trim
	form.Set("sync_soft_drift", "300")      // no unit
	form.Set("server_url", "not-a-title-x") // still just non-blank, but keep browser_origins field distinctly wrong below
	form.Set("browser_origins", "")

	resp, err := client.PostForm(srv.URL+"/", form)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-rendered form with errors)", resp.StatusCode)
	}
	if strings.Contains(string(body), "Configuration saved") {
		t.Error("invalid submission was accepted as a success")
	}
	// Submitted (bad) values must be preserved in the re-render, not reset
	// to defaults, so the operator doesn't lose everything else they typed.
	if !strings.Contains(string(body), `value="1500ms"`) {
		t.Error("re-rendered form lost a field that was actually valid (sync_hard_drift)")
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("expected no config.jsonc written, stat err = %v", err)
	}
}

func TestSetupWizard_PostValidValues_WritesFileSignalsAndShowsSuccess(t *testing.T) {
	srv, configPath, configWritten := newSetupWizardServer(t)
	defer srv.Close()

	client := newJarClient(t)
	_, getBody := mustGetBody(t, client, srv.URL+"/")
	token := extractCSRFToken(t, getBody)

	form := validWizardForm()
	form.Set("csrf_token", token)

	resp, err := client.PostForm(srv.URL+"/", form)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Configuration saved") {
		t.Errorf("response doesn't look like the success page:\n%s", body)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading written config.jsonc: %v", err)
	}
	if !strings.Contains(string(raw), `"title": "Test Party"`) {
		t.Errorf("written config.jsonc missing expected title:\n%s", raw)
	}

	select {
	case <-configWritten:
	default:
		t.Error("configWritten channel was not signaled after a successful POST")
	}
}

func TestSetupWizard_CatchAllOtherPaths_Return503(t *testing.T) {
	srv, _, _ := newSetupWizardServer(t)
	defer srv.Close()

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/party/abc"},
		{http.MethodPost, "/api/parties"},
		{http.MethodGet, "/api/me"},
		{http.MethodDelete, "/api/parties/1/playlist/2"},
		{http.MethodGet, "/setup"},
		{http.MethodGet, "/ws/parties/1"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, srv.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("%s %s status = %d, want 503", tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}

// TestSetupWizard_StaticAssets_Served proves the wizard's own page can
// load the app's real CSS -- a deliberate, necessary addition beyond the
// old setup_placeholder.go's pure-503-everything-else behavior, since the
// wizard page (per this project's own "same server-rendered pattern as
// the rest of the frontend" requirement) links /static/css/style.css.
func TestSetupWizard_StaticAssets_Served(t *testing.T) {
	srv, _, _ := newSetupWizardServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/static/css/style.css")
	if err != nil {
		t.Fatalf("GET /static/css/style.css: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRegisterAwaitingRestartRoutes_HealthzOKWizardRoutesGone(t *testing.T) {
	mux := http.NewServeMux()
	RegisterAwaitingRestartRoutes(mux, slog.Default())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("GET /healthz = %d %q, want 200 \"ok\"", resp.StatusCode, body)
	}

	rootResp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	rootBody, _ := io.ReadAll(rootResp.Body)
	rootResp.Body.Close()
	if rootResp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET / status = %d, want 503", rootResp.StatusCode)
	}
	if strings.Contains(string(rootBody), "csrf_token") {
		t.Error("awaiting-restart mode still serves the wizard form; wizard routes must be structurally absent once config.jsonc exists")
	}
}

// --- Revision 2: generated-key no-leak regression tests ---

// TestSetupWizard_GeneratedKey_NeverWrittenToConfigFile is the structural
// no-leak proof for the generated encryption key on the write side,
// mirroring internal/config's TestLoadFile_TokenEncryptionKey_RejectedAsUnknownField
// for the load side. It drives a full, real GET -> POST cycle, captures
// the exact key value the running wizard displayed (the same way an
// operator would read it -- no privileged test-only accessor), and proves
// that value does not appear anywhere in the bytes actually written to
// config.jsonc.
func TestSetupWizard_GeneratedKey_NeverWrittenToConfigFile(t *testing.T) {
	srv, configPath, _ := newSetupWizardServer(t)
	defer srv.Close()

	client := newJarClient(t)
	_, getBody := mustGetBody(t, client, srv.URL+"/")
	token := extractCSRFToken(t, getBody)
	key := extractGeneratedKey(t, getBody)
	if len(key) < 20 {
		t.Fatalf("extracted generated key looks too short to be real: %q", key)
	}

	form := validWizardForm()
	form.Set("csrf_token", token)
	resp, err := client.PostForm(srv.URL+"/", form)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST / status = %d, want 200", resp.StatusCode)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading written config.jsonc: %v", err)
	}
	if strings.Contains(string(raw), key) {
		t.Errorf("the generated encryption key was found inside the written config.jsonc -- it must never be written to disk:\n%s", raw)
	}
}

// TestSetupWizard_GeneratedKey_NeverLogged mirrors CLAUDE.md's stated
// enforcement mechanism for this class of value ("no automated redaction;
// enforced by not putting these values in a log call in the first
// place") with an automated check that the discipline actually held:
// every log call made across a full GET -> invalid POST -> valid POST
// cycle is captured, and the generated key's exact value must never
// appear in any of it.
func TestSetupWizard_GeneratedKey_NeverLogged(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.jsonc")
	configWritten := make(chan struct{}, 1)
	mux := http.NewServeMux()
	if err := RegisterSetupWizardRoutes(mux, logger, configPath, configWritten); err != nil {
		t.Fatalf("RegisterSetupWizardRoutes: %v", err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := newJarClient(t)
	_, getBody := mustGetBody(t, client, srv.URL+"/")
	token := extractCSRFToken(t, getBody)
	key := extractGeneratedKey(t, getBody)
	if len(key) < 20 {
		t.Fatalf("extracted generated key looks too short to be real: %q", key)
	}

	// An invalid submission first, to exercise the re-render/error log
	// paths too, not just the happy path.
	badForm := validWizardForm()
	badForm.Set("csrf_token", token)
	badForm.Set("sync_soft_drift", "300")
	badResp, err := client.PostForm(srv.URL+"/", badForm)
	if err != nil {
		t.Fatalf("first POST /: %v", err)
	}
	badResp.Body.Close()

	_, getBody2 := mustGetBody(t, client, srv.URL+"/")
	token2 := extractCSRFToken(t, getBody2)
	goodForm := validWizardForm()
	goodForm.Set("csrf_token", token2)
	goodResp, err := client.PostForm(srv.URL+"/", goodForm)
	if err != nil {
		t.Fatalf("second POST /: %v", err)
	}
	goodResp.Body.Close()

	if strings.Contains(logBuf.String(), key) {
		t.Errorf("the generated encryption key appeared in log output:\n%s", logBuf.String())
	}
}
