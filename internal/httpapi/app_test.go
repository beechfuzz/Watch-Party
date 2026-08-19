package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/beechfuzz/watch-party/internal/cryptox"
	"github.com/beechfuzz/watch-party/internal/dbx"
	"github.com/beechfuzz/watch-party/internal/emby"
	"github.com/beechfuzz/watch-party/internal/embyreport"
	"github.com/beechfuzz/watch-party/internal/party"
	"github.com/beechfuzz/watch-party/internal/session"
	"github.com/beechfuzz/watch-party/internal/wsproto"
)

// newFakeEmby returns an httptest server implementing just enough of the
// Emby API (per the Phase 0 findings) for these tests: auth, item lookup,
// playback info, and no-op session reporting endpoints. If capturePlaybackInfoBody
// is non-nil, each /PlaybackInfo request body is JSON-decoded into it —
// for tests asserting on what Watch Party sent Emby, not just what Emby
// sent back.
func newFakeEmby(t *testing.T, capturePlaybackInfoBody *map[string]any) *httptest.Server {
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
	mux.HandleFunc("/Users/user-alice/Items", bulkItemsHandler(map[string]map[string]any{
		"item1": {"Id": "item1", "Name": "Movie", "RunTimeTicks": int64(1200000000)},
	}))
	mux.HandleFunc("/Items/item1/PlaybackInfo", func(w http.ResponseWriter, r *http.Request) {
		if capturePlaybackInfoBody != nil {
			json.NewDecoder(r.Body).Decode(capturePlaybackInfoBody)
		}
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

// bulkItemsHandler serves GET /Users/{userId}/Items?Ids=... — the endpoint
// emby.Client.GetItems uses for the playlist batch-add handler — from a
// small item registry. Only requested ids present in items come back,
// exactly like a real Emby server would omit an id the requester can't see;
// tests simulate "one of the selected items is inaccessible" by simply not
// including it in the map.
func bulkItemsHandler(items map[string]map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ids := strings.Split(r.URL.Query().Get("Ids"), ",")
		out := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			if it, ok := items[id]; ok {
				out = append(out, it)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"Items": out})
	}
}

func newTestApp(t *testing.T) (*App, *httptest.Server) {
	t.Helper()
	fakeEmbySrv := newFakeEmby(t, nil)
	return newTestAppWithEmby(t, emby.NewClient(fakeEmbySrv.URL))
}

// newTestAppWithEmby is newTestApp but with a caller-supplied Emby client —
// for tests that need to inspect what Watch Party sends Emby (via a fake
// Emby server built with a non-nil capturePlaybackInfoBody in newFakeEmby),
// not just assert on the app's own responses.
func newTestAppWithEmby(t *testing.T, embyClient *emby.Client) (*App, *httptest.Server) {
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
	app.Sessions = session.NewManager(store, time.Hour, 24*time.Hour)

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

// addPlaylistItemViaBatch adds a single item through
// POST /api/parties/{id}/playlist/batch and unwraps the one resulting item
// from its {"items": [...]} response — the single-item POST
// /api/parties/{id}/playlist endpoint this replaced was removed once the
// browse dialog's checkbox → batch-add flow became the only production
// caller of playlist adds (see the plan's "Remove handleAddPlaylistItem"
// note), so this helper keeps test call sites for the common single-item
// case simple.
func addPlaylistItemViaBatch(t *testing.T, c *testClient, partyID, itemID string) (*http.Response, map[string]any) {
	t.Helper()
	resp, body := c.do("POST", "/api/parties/"+partyID+"/playlist/batch", map[string]any{"item_ids": []string{itemID}}, true)
	if resp.StatusCode != http.StatusCreated {
		return resp, body
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("batch add response missing items: %v", body)
	}
	first, _ := items[0].(map[string]any)
	return resp, first
}

// createPartyWithCurrentItem drives the multi-step flow that replaced the
// old single-request "create with item_id" design: create a party by name,
// add itemID to its (now-empty-at-creation) playlist, and select it as the
// current item. Returns the new party's id.
func createPartyWithCurrentItem(t *testing.T, c *testClient, name, itemID string) string {
	t.Helper()
	resp, created := c.do("POST", "/api/parties", map[string]string{"name": name}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create party: status = %d body=%v", resp.StatusCode, created)
	}
	partyID, ok := created["party_id"].(string)
	if !ok {
		t.Fatalf("create party: missing party_id in response: %v", created)
	}
	resp2, added := addPlaylistItemViaBatch(t, c, partyID, itemID)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("add playlist item: status = %d body=%v", resp2.StatusCode, added)
	}
	playlistItemID := int64(added["id"].(float64))
	resp3, playBody := c.do("POST", fmt.Sprintf("/api/parties/%s/playlist/%d/play", partyID, playlistItemID), nil, true)
	if resp3.StatusCode != http.StatusNoContent {
		t.Fatalf("select playlist item: status = %d body=%v", resp3.StatusCode, playBody)
	}
	return partyID
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
	resp, body := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, false /* no csrf header */)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, body = %v, want 403", resp.StatusCode, body)
	}
}

func TestCSRF_AcceptedWithValidToken(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	resp, body := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
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
	// A party now starts with a user-chosen name and an empty playlist --
	// no Emby item is required (or possible) at creation time anymore.
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)

	resp, created := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	partyID := created["party_id"].(string)

	resp2, got := c.do("GET", "/api/parties/"+partyID, nil, false)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d body=%v", resp2.StatusCode, got)
	}
	if got["name"] != "Movie Night" {
		t.Errorf("name = %v, want %q", got["name"], "Movie Night")
	}
	if got["item_id"] != "" && got["item_id"] != nil {
		t.Errorf("item_id = %v, want empty (idle, nothing loaded yet)", got["item_id"])
	}
}

func TestPlaybackURL_UsesRequestingUsersOwnToken(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	partyID := createPartyWithCurrentItem(t, c, "Movie Night", "item1")

	resp, got := c.do("GET", "/api/parties/"+partyID+"/playback-url", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
	// lowercase "url": this is what player.js actually reads. A previous
	// version of PlaybackURLResult had no JSON tags at all (serializing as
	// "URL"), which this test asserted on without noticing it didn't match
	// the frontend — silently breaking every login. Assert the real
	// contract here instead of whatever the server happens to emit.
	u, ok := got["url"].(string)
	if !ok || !strings.Contains(u, "api_key=fake-token") {
		t.Errorf("playback URL missing expected api_key: %v", got)
	}
	if _, ok := got["is_transcoded"]; !ok {
		t.Errorf("response missing is_transcoded (player.js needs this to decide direct playback vs. HLS): %v", got)
	}
}

func TestPlaybackURL_StartsAtPartysCurrentPosition(t *testing.T) {
	// Someone requesting a playback URL for a party already in progress
	// (the common case for anyone but the very first person to load the
	// page) must have their Emby transcode session started at roughly
	// where the party actually is now, not the beginning -- otherwise
	// Emby has to work forward through everything that's already played
	// before it can serve the current position. See ARCHITECTURE.md §5.4.
	var lastPlaybackInfoBody map[string]any
	fakeEmbySrv := newFakeEmby(t, &lastPlaybackInfoBody)
	app, srv := newTestAppWithEmby(t, emby.NewClient(fakeEmbySrv.URL))

	c := loginTestClient(t, srv)
	partyID := createPartyWithCurrentItem(t, c, "Movie Night", "item1")

	p, ok := app.Hub.Get(partyID)
	if !ok {
		t.Fatal("party not found in hub")
	}
	if err := p.HandleControl("user-alice", wsproto.MsgPlay, 5*10_000_000); err != nil {
		t.Fatalf("host play: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	resp, got := c.do("GET", "/api/parties/"+partyID+"/playback-url", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}

	sentStartTicks, ok := lastPlaybackInfoBody["StartTimeTicks"].(float64)
	if !ok {
		t.Fatalf("PlaybackInfo request never sent StartTimeTicks: %v", lastPlaybackInfoBody)
	}
	if sentStartTicks <= 5*10_000_000 {
		t.Errorf("StartTimeTicks sent to Emby = %v, want > %d (playing, time elapsed since the 5s play command)", sentStartTicks, 5*10_000_000)
	}
}

// TestPlaybackURL_ReAuthorizedPerCurrentItem covers the playlist-era
// version of "media authorization must be re-validated per participant":
// unlike the old single-item design, the current item can change after
// join, so this re-check must fire again at handlePlaybackURL every time
// it does -- and it must be per-participant, not party-wide. Alice (the
// host) is denied Emby access to the new item; Bob, who joined earlier and
// has separate access, must be unaffected.
func TestPlaybackURL_ReAuthorizedPerCurrentItem(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "fake-token",
			"User":        map[string]string{"Id": "user-alice", "Name": "Alice"},
		})
	})
	mux.HandleFunc("/Users/user-alice/Items/item1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Id": "item1", "Name": "Movie", "RunTimeTicks": int64(1200000000)})
	})
	mux.HandleFunc("/Users/user-alice/Items/item2", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Id": "item2", "Name": "Show", "RunTimeTicks": int64(600000000)})
	})
	mux.HandleFunc("/Users/user-alice/Items", bulkItemsHandler(map[string]map[string]any{
		"item1": {"Id": "item1", "Name": "Movie", "RunTimeTicks": int64(1200000000)},
		"item2": {"Id": "item2", "Name": "Show", "RunTimeTicks": int64(600000000)},
	}))
	mux.HandleFunc("/Items/item1/PlaybackInfo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "sess1",
			"MediaSources":  []map[string]any{{"Id": "src1", "Container": "mp4", "SupportsDirectStream": true}},
		})
	})
	mux.HandleFunc("/Items/item2/PlaybackInfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") == "fake-token" {
			// Alice specifically cannot access item2 (parental control,
			// per-user library restriction, etc).
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "sess2",
			"MediaSources":  []map[string]any{{"Id": "src2", "Container": "mp4", "SupportsDirectStream": true}},
		})
	})
	mux.HandleFunc("/Sessions/Playing", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	fakeEmbySrv := httptest.NewServer(mux)
	t.Cleanup(fakeEmbySrv.Close)

	app, appSrv := newTestAppWithEmby(t, emby.NewClient(fakeEmbySrv.URL))
	alice := loginTestClient(t, appSrv)
	partyID := createPartyWithCurrentItem(t, alice, "Movie Night", "item1")

	if resp, got := alice.do("GET", "/api/parties/"+partyID+"/playback-url", nil, false); resp.StatusCode != http.StatusOK {
		t.Fatalf("alice item1 playback-url: status = %d body=%v", resp.StatusCode, got)
	}

	bobToken, err := app.TokenCipher.Encrypt("bob-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Store.UpsertUser(context.Background(), "user-bob", "Bob", bobToken, time.Now()); err != nil {
		t.Fatal(err)
	}
	bob := newTestClientFor(t, appSrv)
	rec := httptest.NewRecorder()
	fakeReq := httptest.NewRequest("POST", "/", nil)
	sess, err := app.Sessions.Create(context.Background(), rec, fakeReq, "user-bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range rec.Result().Cookies() {
		bob.http.Jar.SetCookies(nil, []*http.Cookie{ck})
	}
	bob.csrfToken = sess.CSRFToken

	// Host switches the party's current item to item2.
	respAdd, added := addPlaylistItemViaBatch(t, alice, partyID, "item2")
	if respAdd.StatusCode != http.StatusCreated {
		t.Fatalf("add item2: status = %d body=%v", respAdd.StatusCode, added)
	}
	playlistItemID := int64(added["id"].(float64))
	respPlay, playBody := alice.do("POST", fmt.Sprintf("/api/parties/%s/playlist/%d/play", partyID, playlistItemID), nil, true)
	if respPlay.StatusCode != http.StatusNoContent {
		t.Fatalf("select item2: status = %d body=%v", respPlay.StatusCode, playBody)
	}

	// Alice is now denied for the new current item...
	if resp, got := alice.do("GET", "/api/parties/"+partyID+"/playback-url", nil, false); resp.StatusCode == http.StatusOK {
		t.Errorf("alice should be denied playback of item2, got 200: %v", got)
	}
	// ...but Bob, who has separate access, still gets a real playback URL.
	if resp, got := bob.do("GET", "/api/parties/"+partyID+"/playback-url", nil, false); resp.StatusCode != http.StatusOK {
		t.Fatalf("bob item2 playback-url: status = %d body=%v", resp.StatusCode, got)
	}
}

func TestListParties_ReturnsActivePartySummary(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	partyID := createPartyWithCurrentItem(t, c, "Movie Night", "item1")

	resp, got := c.do("GET", "/api/parties", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
	parties, ok := got["parties"].([]any)
	if !ok || len(parties) != 1 {
		t.Fatalf("parties = %v, want exactly one entry", got["parties"])
	}
	entry := parties[0].(map[string]any)
	if entry["party_id"] != partyID {
		t.Errorf("party_id = %v, want %v", entry["party_id"], partyID)
	}
	if entry["item_title"] != "Movie" {
		t.Errorf("item_title = %v, want %q (from the fake Emby's item1 response)", entry["item_title"], "Movie")
	}
	if entry["host_display_name"] != "Alice" {
		t.Errorf("host_display_name = %v, want %q", entry["host_display_name"], "Alice")
	}
	if entry["host_user_id"] != "user-alice" {
		t.Errorf("host_user_id = %v, want user-alice", entry["host_user_id"])
	}
	if _, ok := entry["duration_ticks"]; !ok {
		t.Error("response missing duration_ticks")
	}
	if _, ok := entry["member_count"]; !ok {
		t.Error("response missing member_count")
	}
}

func TestListParties_EndedPartyExcluded(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	_, created := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
	partyID := created["party_id"].(string)

	if resp, body := c.do("POST", "/api/parties/"+partyID+"/end", nil, true); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("end party: status = %d body=%v", resp.StatusCode, body)
	}

	resp, got := c.do("GET", "/api/parties", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
	if parties, _ := got["parties"].([]any); len(parties) != 0 {
		t.Errorf("parties = %v, want none listed once the party has ended", parties)
	}
}

func TestStaticAssetsAndPages_NotCacheable(t *testing.T) {
	// This app has no frontend build pipeline: static assets are served at
	// fixed URLs with no content hash, so a new deploy overwrites the same
	// URL's content in place. Without an explicit Cache-Control, a browser
	// or an intermediate CDN caching by file extension (a real default for
	// some CDNs, regardless of what the origin sends) can keep serving a
	// pre-deploy JS/CSS file indefinitely -- see ARCHITECTURE.md §5.6.
	_, srv := newTestApp(t)
	httpClient := &http.Client{}

	for _, path := range []string{"/static/js/player.js", "/static/css/style.css", "/", "/party/some-id"} {
		resp, err := httpClient.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want %q", path, got, "no-cache")
		}
	}
}

func TestGetParty_IncludesItemTitle(t *testing.T) {
	// The party page's topbar needs a human-readable title, not just the
	// Emby item ID -- rides alongside the snapshot fields (see
	// handleGetParty) using the item this endpoint already fetches for its
	// own access check, at no extra Emby round trip.
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	partyID := createPartyWithCurrentItem(t, c, "Movie Night", "item1")

	resp, got := c.do("GET", "/api/parties/"+partyID, nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
	if got["item_title"] != "Movie" {
		t.Errorf("item_title = %v, want %q", got["item_title"], "Movie")
	}
	if _, ok := got["party_id"]; !ok {
		t.Error("response lost existing snapshot fields (party_id missing)")
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
	_, created := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
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
	fakeReq := httptest.NewRequest("POST", "/", nil)
	sess, err := app.Sessions.Create(context.Background(), rec, fakeReq, "user-bob")
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

// bobSession creates a second user directly (bypassing the fake Emby's
// alice-only login) and returns a testClient authenticated as them, for
// tests that need a non-host participant.
func bobSession(t *testing.T, app *App, srv *httptest.Server) *testClient {
	t.Helper()
	if err := app.Store.UpsertUser(context.Background(), "user-bob", "Bob", "enc", time.Now()); err != nil {
		t.Fatal(err)
	}
	c := newTestClientFor(t, srv)
	rec := httptest.NewRecorder()
	fakeReq := httptest.NewRequest("POST", "/", nil)
	sess, err := app.Sessions.Create(context.Background(), rec, fakeReq, "user-bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range rec.Result().Cookies() {
		c.http.Jar.SetCookies(nil, []*http.Cookie{ck})
	}
	c.csrfToken = sess.CSRFToken
	return c
}

func TestCreateParty_SettingsDefaultWhenOmitted(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	resp, created := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%v", resp.StatusCode, created)
	}
	partyID := created["party_id"].(string)

	resp2, got := c.do("GET", "/api/parties/"+partyID, nil, false)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d body=%v", resp2.StatusCode, got)
	}
	if got["auto_advance"] != true || got["show_next_dialog"] != true || got["autoplay_enabled"] != true {
		t.Errorf("default settings not all true: %v", got)
	}
	if got["autoplay_delay_seconds"].(float64) != 5 {
		t.Errorf("autoplay_delay_seconds = %v, want 5", got["autoplay_delay_seconds"])
	}
}

func TestCreateParty_SettingsRespectedWhenProvided(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	resp, created := c.do("POST", "/api/parties", map[string]any{
		"name": "Movie Night", "auto_advance": false, "show_next_dialog": false,
		"autoplay_enabled": false, "autoplay_delay_seconds": 12,
	}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%v", resp.StatusCode, created)
	}
	partyID := created["party_id"].(string)

	resp2, got := c.do("GET", "/api/parties/"+partyID, nil, false)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d body=%v", resp2.StatusCode, got)
	}
	if got["auto_advance"] != false || got["show_next_dialog"] != false || got["autoplay_enabled"] != false {
		t.Errorf("provided settings not respected: %v", got)
	}
	if got["autoplay_delay_seconds"].(float64) != 12 {
		t.Errorf("autoplay_delay_seconds = %v, want 12", got["autoplay_delay_seconds"])
	}
}

func TestUpdatePartySettings_OnlyHostAllowed(t *testing.T) {
	app, srv := newTestApp(t)
	alice := loginTestClient(t, srv)
	resp, created := alice.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d", resp.StatusCode)
	}
	partyID := created["party_id"].(string)
	bob := bobSession(t, app, srv)

	body := map[string]any{
		"name": "Bob's Name", "auto_advance": false, "show_next_dialog": false,
		"autoplay_enabled": false, "autoplay_delay_seconds": 3,
	}
	if resp, respBody := bob.do("PUT", "/api/parties/"+partyID+"/settings", body, true); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob update settings: status = %d body=%v, want 403", resp.StatusCode, respBody)
	}

	body["name"] = "Alice's Name"
	if resp, respBody := alice.do("PUT", "/api/parties/"+partyID+"/settings", body, true); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("alice update settings: status = %d body=%v", resp.StatusCode, respBody)
	}

	resp2, got := alice.do("GET", "/api/parties/"+partyID, nil, false)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d body=%v", resp2.StatusCode, got)
	}
	if got["name"] != "Alice's Name" {
		t.Errorf("name = %v, want %q", got["name"], "Alice's Name")
	}
	if got["auto_advance"] != false || got["autoplay_delay_seconds"].(float64) != 3 {
		t.Errorf("settings not applied: %v", got)
	}
}

func TestUpdatePartySettings_RejectsNonPositiveDelay(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	_, created := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
	partyID := created["party_id"].(string)

	body := map[string]any{
		"name": "Movie Night", "auto_advance": true, "show_next_dialog": true,
		"autoplay_enabled": true, "autoplay_delay_seconds": 0,
	}
	resp, got := c.do("PUT", "/api/parties/"+partyID+"/settings", body, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v, want 400", resp.StatusCode, got)
	}
}

func TestPlaylistMutation_OnlyHostAllowed(t *testing.T) {
	app, srv := newTestApp(t)
	alice := loginTestClient(t, srv)
	resp, created := alice.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status = %d", resp.StatusCode)
	}
	partyID := created["party_id"].(string)
	bob := bobSession(t, app, srv)

	// Non-host cannot add.
	if resp, body := bob.do("POST", "/api/parties/"+partyID+"/playlist/batch", map[string]any{"item_ids": []string{"item1"}}, true); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob add: status = %d body=%v, want 403", resp.StatusCode, body)
	}

	// Host adds two items.
	resp2, itemA := addPlaylistItemViaBatch(t, alice, partyID, "item1")
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("alice add: status = %d body=%v", resp2.StatusCode, itemA)
	}
	itemAID := int64(itemA["id"].(float64))

	// Non-host cannot select what plays.
	if resp, body := bob.do("POST", fmt.Sprintf("/api/parties/%s/playlist/%d/play", partyID, itemAID), nil, true); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob select: status = %d body=%v, want 403", resp.StatusCode, body)
	}

	// Non-host cannot reorder.
	if resp, body := bob.do("PUT", "/api/parties/"+partyID+"/playlist/order", map[string]any{"ordered_ids": []int64{itemAID}}, true); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob reorder: status = %d body=%v, want 403", resp.StatusCode, body)
	}

	// Non-host cannot remove.
	if resp, body := bob.do("DELETE", fmt.Sprintf("/api/parties/%s/playlist/%d", partyID, itemAID), nil, true); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob remove: status = %d body=%v, want 403", resp.StatusCode, body)
	}

	// Host can do all of the above.
	if resp, body := alice.do("POST", fmt.Sprintf("/api/parties/%s/playlist/%d/play", partyID, itemAID), nil, true); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("alice select: status = %d body=%v", resp.StatusCode, body)
	}

	// Removing the now-current item is rejected regardless of who asks.
	if resp, body := alice.do("DELETE", fmt.Sprintf("/api/parties/%s/playlist/%d", partyID, itemAID), nil, true); resp.StatusCode != http.StatusConflict {
		t.Fatalf("remove current item: status = %d body=%v, want 409", resp.StatusCode, body)
	}
}

func TestGetPlaylist_ListsResolvedItems(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	partyID := createPartyWithCurrentItem(t, c, "Movie Night", "item1")

	resp, got := c.do("GET", "/api/parties/"+partyID+"/playlist", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v", resp.StatusCode, got)
	}
	items, ok := got["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %v, want exactly one entry", got["items"])
	}
	entry := items[0].(map[string]any)
	if entry["title"] != "Movie" {
		t.Errorf("title = %v, want %q", entry["title"], "Movie")
	}
	if entry["is_current"] != true {
		t.Errorf("is_current = %v, want true (this item was selected as current)", entry["is_current"])
	}
	if entry["restricted"] != false {
		t.Errorf("restricted = %v, want false", entry["restricted"])
	}
}

// newBulkFakeEmby is a minimal fake Emby server for tests that only need
// auth plus the bulk Items?Ids=... endpoint (batch playlist-add tests) --
// lighter than newFakeEmby, which also wires up PlaybackInfo/Sessions
// endpoints these tests don't exercise.
func newBulkFakeEmby(t *testing.T, items map[string]map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "fake-token",
			"User":        map[string]string{"Id": "user-alice", "Name": "Alice"},
		})
	})
	mux.HandleFunc("/Users/user-alice/Items", bulkItemsHandler(items))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestHandleBatchAddPlaylistItems_HostOnly(t *testing.T) {
	app, srv := newTestApp(t)
	alice := loginTestClient(t, srv)
	_, created := alice.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
	partyID := created["party_id"].(string)
	bob := bobSession(t, app, srv)

	resp, body := bob.do("POST", "/api/parties/"+partyID+"/playlist/batch", map[string]any{"item_ids": []string{"item1"}}, true)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d body=%v, want 403", resp.StatusCode, body)
	}
}

func TestHandleBatchAddPlaylistItems_Atomicity(t *testing.T) {
	// item-b is deliberately omitted from the fake Emby server's registry --
	// simulating an item the requester can't see (or that doesn't exist),
	// which a real Emby Items?Ids=... call would simply omit rather than
	// error on.
	fakeSrv := newBulkFakeEmby(t, map[string]map[string]any{
		"item-a": {"Id": "item-a", "Name": "A", "RunTimeTicks": int64(100)},
	})
	_, srv := newTestAppWithEmby(t, emby.NewClient(fakeSrv.URL))
	c := loginTestClient(t, srv)
	_, created := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
	partyID := created["party_id"].(string)

	resp, body := c.do("POST", "/api/parties/"+partyID+"/playlist/batch", map[string]any{"item_ids": []string{"item-a", "item-b"}}, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v, want 400 (whole batch rejected when one item is inaccessible)", resp.StatusCode, body)
	}

	listResp, listBody := c.do("GET", "/api/parties/"+partyID+"/playlist", nil, false)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list playlist: status = %d", listResp.StatusCode)
	}
	items, _ := listBody["items"].([]any)
	if len(items) != 0 {
		t.Errorf("playlist = %v, want empty (no partial add on a rejected batch)", items)
	}
}

func TestHandleBatchAddPlaylistItems_Order(t *testing.T) {
	fakeSrv := newBulkFakeEmby(t, map[string]map[string]any{
		"item-a": {"Id": "item-a", "Name": "A", "RunTimeTicks": int64(100)},
		"item-b": {"Id": "item-b", "Name": "B", "RunTimeTicks": int64(200)},
	})
	_, srv := newTestAppWithEmby(t, emby.NewClient(fakeSrv.URL))
	c := loginTestClient(t, srv)
	_, created := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
	partyID := created["party_id"].(string)

	resp, body := c.do("POST", "/api/parties/"+partyID+"/playlist/batch", map[string]any{"item_ids": []string{"item-b", "item-a"}}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body=%v", resp.StatusCode, body)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("items = %v, want 2", body["items"])
	}
	first := items[0].(map[string]any)
	second := items[1].(map[string]any)
	if first["item_id"] != "item-b" || second["item_id"] != "item-a" {
		t.Errorf("order = [%v, %v], want [item-b, item-a] (the caller's request order)", first["item_id"], second["item_id"])
	}
}

func TestHandleBatchAddPlaylistItems_SingleEmbyCall(t *testing.T) {
	var callCount int32
	mux := http.NewServeMux()
	mux.HandleFunc("/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "fake-token",
			"User":        map[string]string{"Id": "user-alice", "Name": "Alice"},
		})
	})
	itemsHandler := bulkItemsHandler(map[string]map[string]any{
		"item-a": {"Id": "item-a", "Name": "A", "RunTimeTicks": int64(100)},
		"item-b": {"Id": "item-b", "Name": "B", "RunTimeTicks": int64(200)},
		"item-c": {"Id": "item-c", "Name": "C", "RunTimeTicks": int64(300)},
	})
	mux.HandleFunc("/Users/user-alice/Items", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		itemsHandler(w, r)
	})
	fakeSrv := httptest.NewServer(mux)
	defer fakeSrv.Close()

	_, srv := newTestAppWithEmby(t, emby.NewClient(fakeSrv.URL))
	c := loginTestClient(t, srv)
	_, created := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
	partyID := created["party_id"].(string)

	resp, body := c.do("POST", "/api/parties/"+partyID+"/playlist/batch", map[string]any{"item_ids": []string{"item-a", "item-b", "item-c"}}, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body=%v", resp.StatusCode, body)
	}
	if got := atomic.LoadInt32(&callCount); got != 1 {
		t.Errorf("Emby Items calls = %d, want exactly 1 for a 3-item batch (not N)", got)
	}
}

func TestHandleListSeasons(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "fake-token",
			"User":        map[string]string{"Id": "user-alice", "Name": "Alice"},
		})
	})
	mux.HandleFunc("/Shows/series-1/Seasons", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{
			{"Id": "season-1", "Name": "Season 1", "IndexNumber": 1, "SeriesName": "Andor"},
		}})
	})
	fakeSrv := httptest.NewServer(mux)
	defer fakeSrv.Close()

	_, srv := newTestAppWithEmby(t, emby.NewClient(fakeSrv.URL))
	c := loginTestClient(t, srv)

	resp, body := c.do("GET", "/api/emby/series/series-1/seasons", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v", resp.StatusCode, body)
	}
	seasons, ok := body["seasons"].([]any)
	if !ok || len(seasons) != 1 {
		t.Fatalf("seasons = %v, want exactly 1", body["seasons"])
	}
	season := seasons[0].(map[string]any)
	if season["id"] != "season-1" || season["series_name"] != "Andor" {
		t.Errorf("season = %v", season)
	}
}

func TestHandleListEpisodes(t *testing.T) {
	var gotSeasonID string
	mux := http.NewServeMux()
	mux.HandleFunc("/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "fake-token",
			"User":        map[string]string{"Id": "user-alice", "Name": "Alice"},
		})
	})
	mux.HandleFunc("/Shows/series-1/Episodes", func(w http.ResponseWriter, r *http.Request) {
		gotSeasonID = r.URL.Query().Get("seasonId")
		json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{
			{"Id": "ep-1", "Name": "Episode One", "IndexNumber": 1, "ParentIndexNumber": 1, "SeriesName": "Andor", "RunTimeTicks": int64(360000000)},
		}})
	})
	fakeSrv := httptest.NewServer(mux)
	defer fakeSrv.Close()

	_, srv := newTestAppWithEmby(t, emby.NewClient(fakeSrv.URL))
	c := loginTestClient(t, srv)

	resp, body := c.do("GET", "/api/emby/seasons/season-1/episodes?series_id=series-1", nil, false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%v", resp.StatusCode, body)
	}
	if gotSeasonID != "season-1" {
		t.Errorf("seasonId query param sent to Emby = %q, want season-1", gotSeasonID)
	}
	episodes, ok := body["episodes"].([]any)
	if !ok || len(episodes) != 1 {
		t.Fatalf("episodes = %v, want exactly 1", body["episodes"])
	}
	ep := episodes[0].(map[string]any)
	if ep["run_time_ticks"].(float64) != 360000000 {
		t.Errorf("run_time_ticks = %v, want 360000000 (from the listing response directly, no extra call)", ep["run_time_ticks"])
	}
}

func TestHandleListEpisodes_RequiresSeriesID(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	resp, body := c.do("GET", "/api/emby/seasons/season-1/episodes", nil, false)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d body=%v, want 400 when series_id is omitted", resp.StatusCode, body)
	}
}

func TestWebSocket_OriginRejected(t *testing.T) {
	_, srv := newTestApp(t)
	c := loginTestClient(t, srv)
	_, created := c.do("POST", "/api/parties", map[string]string{"name": "Movie Night"}, true)
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
