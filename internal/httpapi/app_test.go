package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beechfuzz/watch-party/internal/cryptox"
	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/emby"
	"github.com/beechfuzz/watch-party/internal/embyreport"
	"github.com/beechfuzz/watch-party/internal/party"
	"github.com/beechfuzz/watch-party/internal/session"
)

// newFakeEmby returns an httptest server implementing just enough of the
// Emby API (per the Phase 0 findings) for these tests: auth, item lookup,
// playback info, and no-op session reporting endpoints.
func newFakeEmby(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["Username"] != "alice" || body["Pw"] != "hunter2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "fake-token",
			"User":        map[string]string{"Id": "user-alice", "Name": "Alice"},
		})
	})
	mux.HandleFunc("/Users/user-alice/Items/item1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Id": "item1", "Name": "Movie", "RunTimeTicks": int64(1200000000)})
	})
	mux.HandleFunc("/Items/item1/PlaybackInfo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "sess1",
			"MediaSources":  []map[string]any{{"Id": "src1", "Container": "mp4", "SupportsDirectStream": true}},
		})
	})
	mux.HandleFunc("/Sessions/Playing", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("/Sessions/Playing/Progress", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("/Sessions/Playing/Stopped", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	db, err := dbx.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := dbx.NewStore(db)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	key := make([]byte, 32)
	cipher, err := cryptox.NewTokenCipher(key)
	if err != nil {
		t.Fatal(err)
	}

	fakeEmbySrv := newFakeEmby(t)
	embyClient := emby.NewClient(fakeEmbySrv.URL)

	hub := party.NewHub(store, party.Tuning{
		SnapshotInterval: 50 * time.Millisecond, SoftDriftMS: 300, HardDriftMS: 1500,
		MaxRateAdjustment: 0.05, HostGracePeriod: 200 * time.Millisecond,
	}, logger)
	t.Cleanup(func() { hub.Shutdown(context.Background()) })

	reporter := embyreport.New(hub, store, embyClient, cipher, time.Hour, logger)

	app := &App{
		Store: store, Hub: hub, Emby: embyClient, TokenCipher: cipher, Reporter: reporter, Logger: logger,
		AppOrigins: []string{"http://test-origin.example"},
	}
	app.Sessions = session.NewManager(store, time.Hour, 24*time.Hour, true)

	mux := http.NewServeMux()
	RegisterRoutes(mux, app)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return app, srv
}

type testClient struct {
	t         *testing.T
	base      string
	http      *http.Client
	csrfToken string
}

func newTestClientFor(t *testing.T, srv *httptest.Server) *testClient {
	jar := &cookieJar{}
	return &testClient{t: t, base: srv.URL, http: &http.Client{Jar: jar}}
}

// cookieJar is a minimal single-cookie jar (net/http/cookiejar would work
// too, but this avoids the extra import for what's only ever one cookie).
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) { j.cookies = cookies }
func (j *cookieJar) Cookies(u *url.URL) []*http.Cookie             { return j.cookies }

func (c *testClient) do(method, path string, body any, csrf bool) (*http.Response, map[string]any) {
	c.t.Helper()
	var reader *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = strings.NewReader(string(b))
	} else {
		reader = strings.NewReader("")
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set("X-CSRF-Token", c.csrfToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestLogin_Success(t *testing.T) {
	_, srv := newTestApp(t)
	c := newTestClientFor(t, srv)
	resp, body := c.do("POST", "/api/auth/login", map[string]string{"username": "alice", "password": "hunter2"}, false)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["csrf_token"] == "" || body["csrf_token"] == nil {
		t.Errorf("missing csrf_token in response: %v", body)
	}
	user := body["user"].(map[string]any)
	if user["display_name"] != "Alice" {
		t.Errorf("user = %v", user)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	_, srv := newTestApp(t)
	c := newTestClientFor(t, srv)
	resp, body := c.do("POST", "/api/auth/login", map[string]string{"username": "alice", "password": "wrong"}, false)
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
}

func TestMe_RequiresAuth(t *testing.T) {
	_, srv := newTestApp(t)
	c := newTestClientFor(t, srv)
	resp, _ := c.do("GET", "/api/me", nil, false)
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func loginTestClient(t *testing.T, srv *httptest.Server) *testClient {
	t.Helper()
	c := newTestClientFor(t, srv)
	resp, body := c.do("POST", "/api/auth/login", map[string]string{"username": "alice", "password": "hunter2"}, false)
	if resp.StatusCode != 200 {
		t.Fatalf("login failed: %d %v", resp.StatusCode, body)
	}
	c.csrfToken = body["csrf_token"].(string)
	return c
}

func TestMe_WithValidSession(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	resp, body := c.do("GET", "/api/me", nil, false)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
}

func TestCSRF_RejectedWithoutToken(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	resp, body := c.do("POST", "/api/parties", map[string]string{"item_id": "item1"}, false /* no csrf header */)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, body = %v, want 403", resp.StatusCode, body)
	}
}

func TestCSRF_AcceptedWithValidToken(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	resp, body := c.do("POST", "/api/parties", map[string]string{"item_id": "item1"}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["party_id"] == "" {
		t.Errorf("missing party_id: %v", body)
	}
}

func TestLogout_InvalidatesSession(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)

	resp, _ := c.do("POST", "/api/auth/logout", nil, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", resp.StatusCode)
	}

	resp2, _ := c.do("GET", "/api/me", nil, false)
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("status after logout = %d, want 401", resp2.StatusCode)
	}
}

func TestCreateAndGetParty_Roundtrip(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)

	resp, created := c.do("POST", "/api/parties", map[string]string{"item_id": "item1"}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	partyID := created["party_id"].(string)

	resp2, got := c.do("GET", "/api/parties/"+partyID, nil, false)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d body=%v", resp2.StatusCode, got)
	}
	if got["item_id"] != "item1" {
		t.Errorf("got = %v", got)
	}
	if got["duration_ticks"].(float64) != 1200000000 {
		t.Errorf("duration_ticks = %v", got["duration_ticks"])
	}
}

func TestPlaybackURL_UsesRequestingUsersOwnToken(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	_, created := c.do("POST", "/api/parties", map[string]string{"item_id": "item1"}, true)
	partyID := created["party_id"].(string)

	resp, got := c.do("GET", "/api/parties/"+partyID+"/playback-url", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
	u, ok := got["URL"].(string)
	if !ok || !strings.Contains(u, "api_key=fake-token") {
		t.Errorf("playback URL missing expected api_key: %v", got)
	}
}

func TestGetParty_UnknownID_404(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	resp, _ := c.do("GET", "/api/parties/does-not-exist", nil, false)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestEndParty_OnlyHostAllowed(t *testing.T) {
	app, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	_, created := c.do("POST", "/api/parties", map[string]string{"item_id": "item1"}, true)
	partyID := created["party_id"].(string)

	// seed a second user directly and issue them a session to simulate a
	// non-host participant hitting the endpoint.
	if err := app.Store.UpsertUser(context.Background(), "user-bob", "Bob", "enc", time.Now()); err != nil {
		t.Fatal(err)
	}
	bobClient := newTestClientFor(t, srv)
	// Can't log Bob in via fake Emby (only "alice" is accepted there), so
	// create a session directly through the session manager instead.
	rec := httptest.NewRecorder()
	sess, err := app.Sessions.Create(context.Background(), rec, "user-bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.Result().Cookies() {
		bobClient.http.Jar.SetCookies(nil, []*http.Cookie{c})
	}
	bobClient.csrfToken = sess.CSRFToken

	resp, body := bobClient.do("POST", "/api/parties/"+partyID+"/end", nil, true)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, body = %v, want 403 (non-host must not be able to end the party)", resp.StatusCode, body)
	}
}

func TestWebSocket_OriginRejected(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	_, created := c.do("POST", "/api/parties", map[string]string{"item_id": "item1"}, true)
	partyID := created["party_id"].(string)

	wsURL := strings.Replace(srv.URL, "http://", "http://", 1) + "/ws/parties/" + partyID
	req, _ := http.NewRequest("GET", wsURL, nil)
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	for _, ck := range c.http.Jar.Cookies(nil) {
		req.AddCookie(ck)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a disallowed Origin", resp.StatusCode)
	}
}
