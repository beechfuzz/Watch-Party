package main

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// This is the first test file cmd/server has had. It drives runLoop
// directly -- a real temporary config.jsonc path, a real TCP listener per
// incarnation (LISTEN_ADDR=127.0.0.1:0, learned back via the listening
// channel rather than sleeping/polling), real HTTP requests (including a
// real double-submit CSRF cookie round-trip via a cookiejar, exactly as a
// browser would do it), and a test-controlled context in place of real OS
// signals -- see run()'s doc comment for why sending a real SIGTERM here
// would be wrong (it would hit the whole go test binary, not just the
// code under test).
//
// One thing these tests deliberately do NOT claim to prove: that
// privilege-dropping itself (internal/privdrop.Apply, called from
// runNormalWithConfig) behaves correctly when invoked partway through an
// already-running process's lifetime rather than at fresh-process start.
// This test binary sometimes runs as root (privdrop.Apply is only a
// no-op when it isn't), so every test below that reaches
// runNormalWithConfig pins PUID=PGID=0 -- privdrop.Apply then still runs
// its real chown+setuid/setgid syscalls (not skipped), but "drops" from
// root to root, which is a safe no-op rather than an actual privilege
// change. That's deliberate: this file proves the loop's branching and
// transition logic reaches and calls runNormalWithConfig correctly; it is
// not a substitute for the real root-start + real foreign-UID + real
// container manual verification pass documented separately (see the PR
// description / round-2 plan §8.4), which is the only place that actually
// exercises a real privilege drop during this new mid-lifetime sequencing.

var csrfTokenRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// validSetupFormValues returns a complete, valid submission for all 15
// wizard fields -- callers mutate individual keys to test failure paths.
func validSetupFormValues() url.Values {
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

func newTestHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{Jar: jar, Timeout: 5 * time.Second}
}

// fetchCSRFToken does a real GET / and pulls the hidden csrf_token field's
// value out of the rendered HTML, exactly as a browser would hand it back
// on submit -- the CSRF cookie itself is captured automatically by the
// client's cookiejar.
func fetchCSRFToken(t *testing.T, client *http.Client, addr string) string {
	t.Helper()
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading GET / body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200; body:\n%s", resp.StatusCode, body)
	}
	m := csrfTokenRe.FindSubmatch(body)
	if m == nil {
		t.Fatalf("could not find csrf_token in response body:\n%s", body)
	}
	return string(m[1])
}

func waitForAddr(t *testing.T, listening <-chan string, result <-chan error) string {
	t.Helper()
	select {
	case addr := <-listening:
		return addr
	case err := <-result:
		t.Fatalf("runLoop returned (err=%v) before starting a new listener", err)
		return ""
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a server incarnation to start listening")
		return ""
	}
}

// isCgoPrivdropUnsupported reports whether err is privdrop's own
// documented ENOTSUP-under-cgo failure (see internal/privdrop's
// wrapErrno) -- matched precisely on that hint text so this only ever
// catches the one specific, understood environment condition, not any
// other privdrop.Apply failure.
func isCgoPrivdropUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "AllThreadsSyscall cannot safely drop privileges across cgo-created threads")
}

func waitForRunLoopReturn(t *testing.T, result <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(timeout):
		t.Fatal("timed out waiting for runLoop to return")
		return nil
	}
}

// assertRunLoopStillRunning proves runLoop's goroutine has NOT returned
// yet -- i.e. the process did not exit as part of a transition -- by
// giving it a short window in which it must NOT send on result.
func assertRunLoopStillRunning(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("runLoop returned (err=%v) when it should still be serving", err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRunLoop_KeyAlreadySet_TransitionsInProcessWithoutExiting(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.jsonc")

	t.Setenv("LISTEN_ADDR", "127.0.0.1:0")
	t.Setenv("TOKEN_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DATABASE_PATH", filepath.Join(dir, "watchparty.db"))
	// PUID=PGID=0: this test binary sometimes runs as root, in which case
	// runNormalWithConfig's call to privdrop.Apply is NOT a no-op -- it
	// really executes chown + setgid/setuid. Pinning the target to 0:0
	// makes that a safe no-op (root "dropping" to root) instead of
	// permanently changing this test process's real privileges out from
	// under every other test in this binary. See this file's top comment.
	t.Setenv("PUID", "0")
	t.Setenv("PGID", "0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listening := make(chan string, 4)
	result := make(chan error, 1)
	go func() { result <- runLoop(ctx, configPath, listening) }()

	setupAddr := waitForAddr(t, listening, result)

	client := newTestHTTPClient(t)
	csrfToken := fetchCSRFToken(t, client, setupAddr)

	form := validSetupFormValues()
	form.Set("csrf_token", csrfToken)

	resp, err := client.PostForm("http://"+setupAddr+"/", form)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST / status = %d, want 200; body:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Configuration saved") {
		t.Errorf("POST / response doesn't look like the success page:\n%s", body)
	}

	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("expected config.jsonc to exist at %q: %v", configPath, err)
	}

	// A new incarnation binds a new listener next (the old one was closed
	// during the transition) -- unless this test binary happens to have
	// been built with cgo actually linked in, in which case
	// runNormalWithConfig's privdrop.Apply(0:0) call -- a deliberately
	// safe root-to-root no-op, see this file's top comment -- still can't
	// complete, because AllThreadsSyscall unconditionally refuses on any
	// binary that uses cgo, root-to-root or not. This project's own
	// convention is CGO_ENABLED=0 everywhere privdrop matters (see
	// Dockerfile, CLAUDE.md, internal/privdrop's doc comments), so this
	// is treated as an environment mismatch to skip past with a clear
	// reason, not a failure of the loop logic under test -- detected
	// dynamically here (matching privdrop's own wrapErrno hint text
	// exactly) rather than via internal/privdrop's own static
	// `-race`-only raceEnabled check, which this discovery shows doesn't
	// cover every binary that can end up transitively linking cgo (e.g.
	// this one, via net/http, unlike internal/privdrop's own narrower
	// test binary).
	select {
	case normalAddr := <-listening:
		assertRunLoopStillRunning(t, result)

		meResp, err := client.Get("http://" + normalAddr + "/api/me")
		if err != nil {
			t.Fatalf("GET /api/me: %v", err)
		}
		meResp.Body.Close()
		if meResp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET /api/me status = %d, want 401 -- a normal-mode-only route (unauthenticated), proving the mux actually swapped to normal mode", meResp.StatusCode)
		}
	case err := <-result:
		if isCgoPrivdropUnsupported(err) {
			t.Skipf("this test binary links cgo (AllThreadsSyscall unconditionally unsupported, even for the safe root-to-root no-op this test sets up); rebuild/test with CGO_ENABLED=0, this project's documented convention wherever privdrop.Apply runs, to exercise this path: %v", err)
		}
		t.Fatalf("runLoop returned unexpectedly: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the normal-mode listener")
	}

	cancel()
	if err := waitForRunLoopReturn(t, result, 10*time.Second); err != nil {
		t.Errorf("runLoop returned error after ctx cancellation: %v", err)
	}
}

func TestRunLoop_KeyMissing_FallsBackToAwaitingRestart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.jsonc")

	t.Setenv("LISTEN_ADDR", "127.0.0.1:0")
	// Deliberately NOT setting TOKEN_ENCRYPTION_KEY/_FILE.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listening := make(chan string, 4)
	result := make(chan error, 1)
	go func() { result <- runLoop(ctx, configPath, listening) }()

	setupAddr := waitForAddr(t, listening, result)

	client := newTestHTTPClient(t)
	csrfToken := fetchCSRFToken(t, client, setupAddr)

	form := validSetupFormValues()
	form.Set("csrf_token", csrfToken)

	resp, err := client.PostForm("http://"+setupAddr+"/", form)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST / status = %d, want 200; body:\n%s", resp.StatusCode, body)
	}

	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("expected config.jsonc to exist at %q even though the key is missing -- the write itself doesn't depend on the key: %v", configPath, err)
	}

	// New incarnation: runAwaitingRestart, on a new ephemeral port.
	awaitAddr := waitForAddr(t, listening, result)

	assertRunLoopStillRunning(t, result)

	healthzResp, err := client.Get("http://" + awaitAddr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	healthzBody, _ := io.ReadAll(healthzResp.Body)
	healthzResp.Body.Close()
	if healthzResp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", healthzResp.StatusCode)
	}
	if string(healthzBody) != "ok" {
		t.Errorf("GET /healthz body = %q, want \"ok\"", healthzBody)
	}

	rootResp, err := client.Get("http://" + awaitAddr + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	rootBody, _ := io.ReadAll(rootResp.Body)
	rootResp.Body.Close()
	if rootResp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET / status = %d, want 503 (awaiting-restart placeholder, not the wizard form)", rootResp.StatusCode)
	}
	if strings.Contains(string(rootBody), "csrf_token") {
		t.Errorf("GET / still serves the wizard form after config.jsonc was written -- wizard routes must be structurally gone; body:\n%s", rootBody)
	}

	cancel()
	if err := waitForRunLoopReturn(t, result, 10*time.Second); err != nil {
		t.Errorf("runLoop returned error after ctx cancellation: %v", err)
	}
}

func TestRunLoop_InvalidSubmission_NoFileWritten(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.jsonc")

	t.Setenv("LISTEN_ADDR", "127.0.0.1:0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listening := make(chan string, 4)
	result := make(chan error, 1)
	go func() { result <- runLoop(ctx, configPath, listening) }()

	setupAddr := waitForAddr(t, listening, result)

	client := newTestHTTPClient(t)
	csrfToken := fetchCSRFToken(t, client, setupAddr)

	form := validSetupFormValues()
	form.Set("csrf_token", csrfToken)
	form.Set("sync_soft_drift", "300") // no unit -- ParseDurationStrict must reject this

	resp, err := client.PostForm("http://"+setupAddr+"/", form)
	if err != nil {
		t.Fatalf("POST /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST / (invalid submission) status = %d, want 200 (re-rendered form with errors)", resp.StatusCode)
	}
	if strings.Contains(string(body), "Configuration saved") {
		t.Errorf("invalid submission was accepted as if it succeeded:\n%s", body)
	}

	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("expected no config.jsonc to be written for an invalid submission, stat err = %v", err)
	}

	assertRunLoopStillRunning(t, result)

	// Prove the loop is still serving runSetupRequired (not stuck, not
	// crashed) by successfully resubmitting with valid values this time.
	csrfToken2 := fetchCSRFToken(t, client, setupAddr)
	form2 := validSetupFormValues()
	form2.Set("csrf_token", csrfToken2)
	resp2, err := client.PostForm("http://"+setupAddr+"/", form2)
	if err != nil {
		t.Fatalf("second POST /: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !strings.Contains(string(body2), "Configuration saved") {
		t.Fatalf("valid resubmission after an invalid one did not succeed: status=%d body=\n%s", resp2.StatusCode, body2)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("expected config.jsonc to exist after the valid resubmission: %v", err)
	}

	cancel()
	// The loop will attempt runNormalWithConfig or runAwaitingRestart next
	// (TOKEN_ENCRYPTION_KEY isn't set in this test, so it'll be the
	// latter); either way it must still return promptly once cancelled.
	waitForRunLoopReturn(t, result, 10*time.Second)
}
