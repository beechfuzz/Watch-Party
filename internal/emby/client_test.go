package emby

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAuthenticateByName_SendsRequiredHeadersAndParsesResponse(t *testing.T) {
	var gotAuthHeader, gotContentType string
	var gotBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/AuthenticateByName" || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotAuthHeader = r.Header.Get("X-Emby-Authorization")
		gotContentType = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{
			"AccessToken": "tok-123",
			"User":        map[string]string{"Id": "user-1", "Name": "Alice"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	result, err := c.AuthenticateByName(context.Background(), "alice", "hunter2")
	if err != nil {
		t.Fatalf("AuthenticateByName: %v", err)
	}
	if result.AccessToken != "tok-123" || result.UserID != "user-1" || result.DisplayName != "Alice" {
		t.Errorf("result = %+v", result)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if !strings.Contains(gotAuthHeader, `Client="Watch Party"`) || !strings.Contains(gotAuthHeader, "DeviceId=") {
		t.Errorf("X-Emby-Authorization missing required fields: %q", gotAuthHeader)
	}
	if gotBody["Username"] != "alice" || gotBody["Pw"] != "hunter2" {
		t.Errorf("request body = %+v, want Username=alice Pw=hunter2", gotBody)
	}
}

func TestAuthenticateByName_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.AuthenticateByName(context.Background(), "alice", "wrong")
	if err != ErrUnauthorized {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestDeviceID_StableAndDeterministic(t *testing.T) {
	a := DeviceID("alice")
	b := DeviceID("alice")
	c := DeviceID("bob")
	if a != b {
		t.Error("DeviceID should be deterministic for the same username")
	}
	if a == c {
		t.Error("DeviceID should differ for different usernames")
	}
}

func TestGetPlaybackURL_SendsDeviceProfileToLetEmbyDecide(t *testing.T) {
	var gotMethod string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "sess1",
			"MediaSources":  []map[string]any{{"Id": "src1", "Container": "mp4", "SupportsDirectStream": true}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, err := c.GetPlaybackURL(context.Background(), "tok", "user-1", "item1", "device-1"); err != nil {
		t.Fatalf("GetPlaybackURL: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST (a plain GET can't carry a DeviceProfile body)", gotMethod)
	}
	if gotBody["UserId"] != "user-1" {
		t.Errorf("UserId = %v, want user-1", gotBody["UserId"])
	}
	profile, ok := gotBody["DeviceProfile"].(map[string]any)
	if !ok {
		t.Fatal("expected a DeviceProfile in the request body so Emby can tell direct-play-capable browsers apart from ones that need transcoding")
	}
	if _, ok := profile["DirectPlayProfiles"]; !ok {
		t.Error("DeviceProfile missing DirectPlayProfiles")
	}
	if _, ok := profile["TranscodingProfiles"]; !ok {
		t.Error("DeviceProfile missing TranscodingProfiles")
	}
}

func TestGetPlaybackURL_PreferEmbyProvidedTranscodingUrl(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "sess-abc",
			"MediaSources": []map[string]any{
				{
					"Id": "src-abc", "SupportsDirectStream": false, "SupportsDirectPlay": false, "SupportsTranscoding": true,
					"TranscodingUrl": "/Videos/item1/master.m3u8?MediaSourceId=src-abc&PlaySessionId=sess-abc&DeviceId=device-1",
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	result, err := c.GetPlaybackURL(context.Background(), "tok-xyz", "user-1", "item1", "device-1")
	if err != nil {
		t.Fatalf("GetPlaybackURL: %v", err)
	}
	if !result.IsTranscoded {
		t.Fatal("expected transcoded result")
	}
	parsed, err := url.Parse(result.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/Videos/item1/master.m3u8" {
		t.Errorf("path = %q, want the Emby-provided path preserved verbatim", parsed.Path)
	}
	q := parsed.Query()
	if q.Get("MediaSourceId") != "src-abc" || q.Get("PlaySessionId") != "sess-abc" || q.Get("DeviceId") != "device-1" {
		t.Errorf("query lost Emby-provided params: %v", q)
	}
	if q.Get("api_key") != "tok-xyz" {
		t.Errorf("api_key = %q, want it appended to the Emby-provided URL", q.Get("api_key"))
	}
}

func TestGetPlaybackURL_UsesPublicBaseURLForBrowserFacingURLs(t *testing.T) {
	// baseURL (the internal address, e.g. container DNS) is used for the
	// PlaybackInfo call itself; publicBaseURL (a browser-reachable address)
	// must be used for every URL handed back to the browser, both direct
	// and transcoded — otherwise a browser can't resolve it at all. See
	// ARCHITECTURE.md §5.2.
	for _, tc := range []struct {
		name         string
		mediaSource  map[string]any
		wantHostPart string
	}{
		{
			name: "direct stream",
			mediaSource: map[string]any{
				"Id": "src1", "Container": "mp4", "SupportsDirectStream": true, "SupportsDirectPlay": true,
			},
		},
		{
			name: "transcoded, Emby-provided TranscodingUrl",
			mediaSource: map[string]any{
				"Id": "src2", "SupportsDirectStream": false, "SupportsDirectPlay": false, "SupportsTranscoding": true,
				"TranscodingUrl": "/Videos/item1/master.m3u8?MediaSourceId=src2",
			},
		},
		{
			name: "transcoded, no TranscodingUrl, fall back to hand-built master.m3u8",
			mediaSource: map[string]any{
				"Id": "src3", "SupportsDirectStream": false, "SupportsDirectPlay": false, "SupportsTranscoding": true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{
					"PlaySessionId": "sess-1",
					"MediaSources":  []map[string]any{tc.mediaSource},
				})
			}))
			defer srv.Close()

			c := NewClient(srv.URL)
			c.SetPublicBaseURL("http://public.example")
			result, err := c.GetPlaybackURL(context.Background(), "tok", "user-1", "item1", "device-1")
			if err != nil {
				t.Fatalf("GetPlaybackURL: %v", err)
			}
			if !strings.HasPrefix(result.URL, "http://public.example/") {
				t.Errorf("URL = %q, want it built from the public base URL, not %q", result.URL, srv.URL)
			}
		})
	}
}

func TestSetPublicBaseURL_BlankIsNoOp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "sess-1",
			"MediaSources": []map[string]any{
				{"Id": "src1", "Container": "mp4", "SupportsDirectStream": true, "SupportsDirectPlay": true},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	c.SetPublicBaseURL("") // e.g. EMBY_PUBLIC_URL unset
	result, err := c.GetPlaybackURL(context.Background(), "tok", "user-1", "item1", "device-1")
	if err != nil {
		t.Fatalf("GetPlaybackURL: %v", err)
	}
	if !strings.HasPrefix(result.URL, srv.URL+"/") {
		t.Errorf("URL = %q, want it to default to baseURL (%q) when publicBaseURL is never set", result.URL, srv.URL)
	}
}

func TestPlaybackURLResult_WireCasingMatchesFrontend(t *testing.T) {
	// player.js reads playback.url / playback.is_transcoded (snake_case) —
	// this pins the JSON wire shape so a struct-tag regression here would
	// silently break every login again, the way the untagged struct did.
	result := PlaybackURLResult{URL: "http://x/y", IsTranscoded: true, MediaSourceID: "s1", PlaySessionID: "p1", Container: "mp4"}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"url", "is_transcoded", "media_source_id", "play_session_id", "container"} {
		if _, ok := m[key]; !ok {
			t.Errorf("wire JSON missing expected key %q; got keys %v", key, m)
		}
	}
}

func TestGetPlaybackURL_DirectStream_UsesQueryParamToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/item42/PlaybackInfo" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Emby-Token"); got != "tok-abc" {
			t.Errorf("X-Emby-Token = %q", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "play-sess-1",
			"MediaSources": []map[string]any{
				{"Id": "src1", "Container": "mkv", "SupportsDirectStream": true, "SupportsDirectPlay": true},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	result, err := c.GetPlaybackURL(context.Background(), "tok-abc", "user-1", "item42", "device-1")
	if err != nil {
		t.Fatalf("GetPlaybackURL: %v", err)
	}
	if result.IsTranscoded {
		t.Error("expected direct stream, not transcoded")
	}
	if result.MediaSourceID != "src1" || result.PlaySessionID != "play-sess-1" {
		t.Errorf("result = %+v", result)
	}

	parsed, err := url.Parse(result.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(parsed.Path, "/Videos/item42/stream") {
		t.Errorf("path = %q, want /Videos/item42/stream...", parsed.Path)
	}
	if !strings.HasSuffix(parsed.Path, ".mkv") {
		t.Errorf("path = %q, want .mkv suffix matching Container", parsed.Path)
	}
	q := parsed.Query()
	if q.Get("api_key") != "tok-abc" {
		t.Errorf("api_key query param = %q, want the user's own token (required since <video src> can't set headers)", q.Get("api_key"))
	}
	if q.Get("Static") != "true" {
		t.Errorf("Static = %q, want true", q.Get("Static"))
	}
	if q.Get("MediaSourceId") != "src1" || q.Get("PlaySessionId") != "play-sess-1" {
		t.Errorf("query = %v", q)
	}
}

func TestGetPlaybackURL_Transcoded_BuildsHLSUrl(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "play-sess-2",
			"MediaSources": []map[string]any{
				{"Id": "src2", "Container": "avi", "SupportsDirectStream": false, "SupportsDirectPlay": false, "SupportsTranscoding": true},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	result, err := c.GetPlaybackURL(context.Background(), "tok-xyz", "user-1", "item99", "device-1")
	if err != nil {
		t.Fatalf("GetPlaybackURL: %v", err)
	}
	if !result.IsTranscoded {
		t.Error("expected transcoded result")
	}
	parsed, _ := url.Parse(result.URL)
	if !strings.HasSuffix(parsed.Path, "/Videos/item99/master.m3u8") {
		t.Errorf("path = %q, want .../master.m3u8", parsed.Path)
	}
	q := parsed.Query()
	if q.Get("api_key") != "tok-xyz" || q.Get("DeviceId") != "device-1" {
		t.Errorf("query = %v", q)
	}
}

func TestGetPlaybackURL_NoSupportedMethod_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "s",
			"MediaSources":  []map[string]any{{"Id": "src3"}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.GetPlaybackURL(context.Background(), "tok", "user-1", "item1", "device-1")
	if err == nil {
		t.Error("expected error when no playback method is supported")
	}
}

func TestReportPlaybackProgress_SendsPositionAndPauseState(t *testing.T) {
	var gotBody map[string]any
	var gotPath, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Emby-Token")
		json.NewDecoder(r.Body).Decode(&gotBody)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	err := c.ReportPlaybackProgress(context.Background(), "tok-1", "device-1", PlayingReport{
		ItemID: "item1", MediaSourceID: "src1", PositionTicks: 12345, IsPaused: true,
		PlayMethod: "DirectStream", PlaySessionID: "sess1", CanSeek: true,
	})
	if err != nil {
		t.Fatalf("ReportPlaybackProgress: %v", err)
	}
	if gotPath != "/Sessions/Playing/Progress" {
		t.Errorf("path = %q", gotPath)
	}
	if gotToken != "tok-1" {
		t.Errorf("token = %q", gotToken)
	}
	posTicks, _ := gotBody["PositionTicks"].(float64)
	if int64(posTicks) != 12345 {
		t.Errorf("PositionTicks = %v, want 12345", gotBody["PositionTicks"])
	}
	if gotBody["IsPaused"] != true {
		t.Errorf("IsPaused = %v, want true", gotBody["IsPaused"])
	}
}

func TestGetItem_ParsesRunTimeTicks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/user-1/Items/item7" {
			t.Errorf("path = %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"Id": "item7", "Name": "Movie", "RunTimeTicks": 72000000000})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	item, err := c.GetItem(context.Background(), "tok", "user-1", "item7")
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if item.RunTimeTicks != 72000000000 || item.Name != "Movie" {
		t.Errorf("item = %+v", item)
	}
}
