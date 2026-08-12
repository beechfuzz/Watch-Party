package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/beechfuzz/watch-party/internal/dbx"
)

func testStore(t *testing.T) *dbx.Store {
	t.Helper()
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("dbx.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := dbx.NewStore(db)
	if err := store.UpsertUser(context.Background(), "user1", "Alice", "enc-token", time.Now()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestIsSecureRequest(t *testing.T) {
	cases := []struct {
		name string
		req  *http.Request
		want bool
	}{
		{"nil request", nil, false},
		{"plain http, no forwarded header", httptest.NewRequest("GET", "http://watchparty.home/", nil), false},
		{"X-Forwarded-Proto: https", withHeader(httptest.NewRequest("GET", "http://internal/", nil), "X-Forwarded-Proto", "https"), true},
		{"X-Forwarded-Proto: http", withHeader(httptest.NewRequest("GET", "http://internal/", nil), "X-Forwarded-Proto", "http"), false},
		{"X-Forwarded-Proto case-insensitive", withHeader(httptest.NewRequest("GET", "http://internal/", nil), "X-Forwarded-Proto", "HTTPS"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSecureRequest(c.req); got != c.want {
				t.Errorf("isSecureRequest = %v, want %v", got, c.want)
			}
		})
	}
}

func withHeader(r *http.Request, key, value string) *http.Request {
	r.Header.Set(key, value)
	return r
}

// TestCreate_SecureCookieMatchesRequestScheme is the core of what this
// package needs to get right for a mixed-origin deployment: the same
// Manager, same process, must issue a Secure cookie for a request that
// looks like it came in over HTTPS and a non-Secure cookie for one that
// looks like plain HTTP — a static per-deployment flag can't do this.
func TestCreate_SecureCookieMatchesRequestScheme(t *testing.T) {
	store := testStore(t)
	m := NewManager(store, time.Hour, 24*time.Hour)

	httpsReq := withHeader(httptest.NewRequest("POST", "http://watchparty.example.com/api/auth/login", nil), "X-Forwarded-Proto", "https")
	rec1 := httptest.NewRecorder()
	if _, err := m.Create(context.Background(), rec1, httpsReq, "user1"); err != nil {
		t.Fatal(err)
	}
	cookie1 := findCookie(t, rec1.Result().Cookies())
	if !cookie1.Secure {
		t.Error("cookie for the HTTPS-origin request should be Secure")
	}

	httpReq := httptest.NewRequest("POST", "http://watchparty.home/api/auth/login", nil)
	rec2 := httptest.NewRecorder()
	if _, err := m.Create(context.Background(), rec2, httpReq, "user1"); err != nil {
		t.Fatal(err)
	}
	cookie2 := findCookie(t, rec2.Result().Cookies())
	if cookie2.Secure {
		t.Error("cookie for the plain-HTTP-origin request should not be Secure (browser would refuse to store it otherwise)")
	}
}

func TestLogout_ClearingCookieMatchesRequestScheme(t *testing.T) {
	store := testStore(t)
	m := NewManager(store, time.Hour, 24*time.Hour)

	httpReq := httptest.NewRequest("POST", "http://watchparty.home/api/auth/logout", nil)
	rec := httptest.NewRecorder()
	if err := m.Logout(context.Background(), rec, httpReq, "some-session-id"); err != nil {
		// DeleteSession on a nonexistent id is a no-op in the store, not an error
		t.Fatalf("Logout: %v", err)
	}
	cookie := findCookie(t, rec.Result().Cookies())
	if cookie.Secure {
		t.Error("clearing cookie for a plain-HTTP request should not be Secure")
	}
	if cookie.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative (deletes the cookie)", cookie.MaxAge)
	}
}

func findCookie(t *testing.T, cookies []*http.Cookie) *http.Cookie {
	t.Helper()
	for _, c := range cookies {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie found among %v", CookieName, cookies)
	return nil
}

func TestCreateAuthenticateLogout_RoundTrip(t *testing.T) {
	store := testStore(t)
	m := NewManager(store, time.Hour, 24*time.Hour)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "http://watchparty.home/api/auth/login", nil)
	sess, err := m.Create(context.Background(), rec, req, "user1")
	if err != nil {
		t.Fatal(err)
	}
	if sess.CSRFToken == "" {
		t.Error("expected a non-empty CSRF token")
	}

	authReq := httptest.NewRequest("GET", "http://watchparty.home/api/me", nil)
	authReq.AddCookie(findCookie(t, rec.Result().Cookies()))

	got, err := m.Authenticate(context.Background(), authReq)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.UserID != "user1" || got.ID != sess.ID {
		t.Errorf("got = %+v", got)
	}

	logoutRec := httptest.NewRecorder()
	if err := m.Logout(context.Background(), logoutRec, authReq, got.ID); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if _, err := m.Authenticate(context.Background(), authReq); err != ErrNoSession {
		t.Errorf("Authenticate after logout = %v, want ErrNoSession", err)
	}
}

func TestAuthenticate_NoCookie(t *testing.T) {
	store := testStore(t)
	m := NewManager(store, time.Hour, 24*time.Hour)
	req := httptest.NewRequest("GET", "http://watchparty.home/api/me", nil)
	if _, err := m.Authenticate(context.Background(), req); err != ErrNoSession {
		t.Errorf("err = %v, want ErrNoSession", err)
	}
}

func TestAuthenticate_ExpiredByIdleTimeout(t *testing.T) {
	store := testStore(t)
	m := NewManager(store, 10*time.Millisecond, 24*time.Hour)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "http://watchparty.home/api/auth/login", nil)
	if _, err := m.Create(context.Background(), rec, req, "user1"); err != nil {
		t.Fatal(err)
	}

	authReq := httptest.NewRequest("GET", "http://watchparty.home/api/me", nil)
	authReq.AddCookie(findCookie(t, rec.Result().Cookies()))

	time.Sleep(30 * time.Millisecond)
	if _, err := m.Authenticate(context.Background(), authReq); err != ErrSessionExpired {
		t.Errorf("err = %v, want ErrSessionExpired", err)
	}
}

func TestValidateCSRF(t *testing.T) {
	sess := &dbx.Session{CSRFToken: "secret-token"}

	good := httptest.NewRequest("POST", "/", nil)
	good.Header.Set(CSRFHeaderName, "secret-token")
	if err := ValidateCSRF(good, sess); err != nil {
		t.Errorf("expected valid CSRF to pass, got %v", err)
	}

	bad := httptest.NewRequest("POST", "/", nil)
	bad.Header.Set(CSRFHeaderName, "wrong-token")
	if err := ValidateCSRF(bad, sess); err != ErrCSRFMismatch {
		t.Errorf("err = %v, want ErrCSRFMismatch", err)
	}

	missing := httptest.NewRequest("POST", "/", nil)
	if err := ValidateCSRF(missing, sess); err != ErrCSRFMismatch {
		t.Errorf("err = %v, want ErrCSRFMismatch for missing header", err)
	}
}
