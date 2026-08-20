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
	if _, err := c.GetPlaybackURL(context.Background(), "tok", "user-1", "item1", "device-1", 0); err != nil {
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

func TestGetPlaybackURL_StartPositionTicks_SentToEmbyAndReturnedForTranscoded(t *testing.T) {
	// A nonzero startPositionTicks tells Emby to begin the transcode at
	// that offset (instead of the beginning) -- essential for anyone
	// joining a party already in progress, so Emby doesn't have to
	// transcode through everything that already played before it can
	// serve the current position. See ARCHITECTURE.md §5.4.
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "sess1",
			"MediaSources": []map[string]any{
				{"Id": "src1", "SupportsDirectStream": false, "SupportsDirectPlay": false, "SupportsTranscoding": true},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	result, err := c.GetPlaybackURL(context.Background(), "tok", "user-1", "item1", "device-1", 123456789)
	if err != nil {
		t.Fatalf("GetPlaybackURL: %v", err)
	}
	if got := gotBody["StartTimeTicks"]; got != float64(123456789) {
		t.Errorf("PlaybackInfo request StartTimeTicks = %v, want 123456789", got)
	}
	if result.StartPositionTicks != 123456789 {
		t.Errorf("result.StartPositionTicks = %d, want 123456789 (frontend needs this to translate video.currentTime to/from the item's real position)", result.StartPositionTicks)
	}
	parsed, err := url.Parse(result.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("StartTimeTicks"); got != "123456789" {
		t.Errorf("hand-built master.m3u8 URL StartTimeTicks = %q, want 123456789", got)
	}
}

func TestGetPlaybackURL_DirectStream_StartPositionTicksAlwaysZero(t *testing.T) {
	// Direct play/stream serves the file as-is: video.currentTime is
	// already an absolute item position (the browser Range-requests
	// wherever it seeks to), so there's no timeline offset to track,
	// regardless of what startPositionTicks the caller passed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "sess1",
			"MediaSources": []map[string]any{
				{"Id": "src1", "Container": "mp4", "SupportsDirectStream": true, "SupportsDirectPlay": true},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	result, err := c.GetPlaybackURL(context.Background(), "tok", "user-1", "item1", "device-1", 123456789)
	if err != nil {
		t.Fatalf("GetPlaybackURL: %v", err)
	}
	if result.StartPositionTicks != 0 {
		t.Errorf("result.StartPositionTicks = %d, want 0 for direct stream", result.StartPositionTicks)
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
	result, err := c.GetPlaybackURL(context.Background(), "tok-xyz", "user-1", "item1", "device-1", 0)
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
		t.Errorf("api_key = %q, want it set on the Emby-provided URL", q.Get("api_key"))
	}
}

func TestGetPlaybackURL_TranscodingUrlAlreadyHasAPIKey_NoDuplicate(t *testing.T) {
	// Emby's own TranscodingUrl commonly already embeds an api_key, from
	// whatever token authenticated the PlaybackInfo call that returned it.
	// Blindly appending our own produced two api_key params on the wire,
	// which Emby rejected outright with 401 (couldn't reconcile two values
	// for the same key into one token) -- a real deployment hit exactly
	// this. See ARCHITECTURE.md §5.3.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"PlaySessionId": "sess-abc",
			"MediaSources": []map[string]any{
				{
					"Id": "src-abc", "SupportsDirectStream": false, "SupportsDirectPlay": false, "SupportsTranscoding": true,
					"TranscodingUrl": "/videos/item1/master.m3u8?MediaSourceId=src-abc&PlaySessionId=sess-abc&api_key=stale-token-from-embys-own-auth",
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	result, err := c.GetPlaybackURL(context.Background(), "tok-xyz", "user-1", "item1", "device-1", 0)
	if err != nil {
		t.Fatalf("GetPlaybackURL: %v", err)
	}
	parsed, err := url.Parse(result.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	if got := q["api_key"]; len(got) != 1 {
		t.Fatalf("api_key values = %v, want exactly one", got)
	}
	if q.Get("api_key") != "tok-xyz" {
		t.Errorf("api_key = %q, want the requesting user's own token, overwriting Emby's stale one", q.Get("api_key"))
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
			result, err := c.GetPlaybackURL(context.Background(), "tok", "user-1", "item1", "device-1", 0)
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
	result, err := c.GetPlaybackURL(context.Background(), "tok", "user-1", "item1", "device-1", 0)
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
	result := PlaybackURLResult{URL: "http://x/y", IsTranscoded: true, MediaSourceID: "s1", PlaySessionID: "p1", Container: "mp4", StartPositionTicks: 42}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"url", "is_transcoded", "media_source_id", "play_session_id", "container", "start_position_ticks"} {
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
	result, err := c.GetPlaybackURL(context.Background(), "tok-abc", "user-1", "item42", "device-1", 0)
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
	result, err := c.GetPlaybackURL(context.Background(), "tok-xyz", "user-1", "item99", "device-1", 0)
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
	_, err := c.GetPlaybackURL(context.Background(), "tok", "user-1", "item1", "device-1", 0)
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

func TestListSeasons_ParsesResponse(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Query().Get("userId") != "user-1" {
			t.Errorf("userId = %q", r.URL.Query().Get("userId"))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"Items": []map[string]any{
				{"Id": "season1", "Name": "Season 1", "IndexNumber": 1, "SeriesName": "Andor"},
				{"Id": "season2", "Name": "Season 2", "IndexNumber": 2, "SeriesName": "Andor", "ImageTags": map[string]string{"Primary": "tag123"}},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	seasons, err := c.ListSeasons(context.Background(), "tok", "user-1", "series-1")
	if err != nil {
		t.Fatalf("ListSeasons: %v", err)
	}
	if gotPath != "/Shows/series-1/Seasons" {
		t.Errorf("path = %q", gotPath)
	}
	if len(seasons) != 2 {
		t.Fatalf("seasons = %+v, want 2", seasons)
	}
	if seasons[0].SeriesID != "series-1" {
		t.Errorf("SeriesID = %q, want the caller's own input (series-1), not trusted from the response", seasons[0].SeriesID)
	}
	if seasons[1].PosterURL == "" {
		t.Error("expected PosterURL to be set when ImageTags.Primary is present")
	}
}

func TestListEpisodes_RunTimeTicksFromListingResponse_NoExtraCall(t *testing.T) {
	// Emby's episode listing already includes RunTimeTicks per item -- this
	// pins that ListEpisodes reads it straight from that response and never
	// issues a follow-up GetItem call per episode (which would be an N+1
	// round trip for a large season).
	var callCount int
	var gotPath, gotSeasonID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		gotPath = r.URL.Path
		gotSeasonID = r.URL.Query().Get("seasonId")
		json.NewEncoder(w).Encode(map[string]any{
			"Items": []map[string]any{
				{"Id": "ep1", "Name": "Episode One", "IndexNumber": 1, "ParentIndexNumber": 1, "SeriesName": "Andor", "RunTimeTicks": int64(360000000)},
				{"Id": "ep2", "Name": "Episode Two", "IndexNumber": 2, "ParentIndexNumber": 1, "SeriesName": "Andor", "RunTimeTicks": int64(372000000)},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	episodes, err := c.ListEpisodes(context.Background(), "tok", "user-1", "series-1", "season-1")
	if err != nil {
		t.Fatalf("ListEpisodes: %v", err)
	}
	if callCount != 1 {
		t.Errorf("callCount = %d, want exactly 1 (no per-episode follow-up call)", callCount)
	}
	if gotPath != "/Shows/series-1/Episodes" {
		t.Errorf("path = %q", gotPath)
	}
	if gotSeasonID != "season-1" {
		t.Errorf("seasonId query param = %q, want season-1", gotSeasonID)
	}
	if len(episodes) != 2 {
		t.Fatalf("episodes = %+v, want 2", episodes)
	}
	if episodes[0].RunTimeTicks != 360000000 || episodes[1].RunTimeTicks != 372000000 {
		t.Errorf("RunTimeTicks not read from listing response: %+v", episodes)
	}
	if episodes[0].SeasonIndexNumber != 1 {
		t.Errorf("SeasonIndexNumber = %d, want 1 (from Emby's ParentIndexNumber field)", episodes[0].SeasonIndexNumber)
	}
	if episodes[0].SeriesID != "series-1" || episodes[0].SeasonID != "season-1" {
		t.Errorf("episode = %+v, want SeriesID/SeasonID set from the caller's own input", episodes[0])
	}
}

func TestGetItems_CommaJoinedIdsAndPartialResponse(t *testing.T) {
	var gotIds string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/user-1/Items" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotIds = r.URL.Query().Get("Ids")
		// Simulate one of the three requested ids being inaccessible to this
		// user: Emby simply omits it from the response, rather than erroring.
		json.NewEncoder(w).Encode(map[string]any{
			"Items": []map[string]any{
				{"Id": "item-a", "Name": "A", "RunTimeTicks": int64(100)},
				{"Id": "item-c", "Name": "C", "RunTimeTicks": int64(300)},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	items, err := c.GetItems(context.Background(), "tok", "user-1", []string{"item-a", "item-b", "item-c"})
	if err != nil {
		t.Fatalf("GetItems: %v", err)
	}
	if gotIds != "item-a,item-b,item-c" {
		t.Errorf("Ids query param = %q, want comma-joined request order", gotIds)
	}
	if len(items) != 2 {
		t.Fatalf("items = %+v, want exactly the 2 accessible items (caller must detect the gap itself)", items)
	}
	if items[0].ID != "item-a" || items[1].ID != "item-c" {
		t.Errorf("items = %+v", items)
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

func TestGetUser_ParsesProfileAndPrimaryImageTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Users/user-2" {
			t.Errorf("path = %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"Id": "user-2", "Name": "Jamie", "PrimaryImageTag": "abc123"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	u, err := c.GetUser(context.Background(), "tok", "user-2")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.ID != "user-2" || u.Name != "Jamie" || !u.HasPrimaryImage {
		t.Errorf("user = %+v, want {user-2 Jamie true}", u)
	}
}

func TestGetUser_NoPrimaryImageTag_HasPrimaryImageFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"Id": "user-3", "Name": "Alex", "PrimaryImageTag": ""})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	u, err := c.GetUser(context.Background(), "tok", "user-3")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.HasPrimaryImage {
		t.Error("HasPrimaryImage = true, want false for a user with no PrimaryImageTag")
	}
}

func TestGetUser_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	if _, err := c.GetUser(context.Background(), "bad-tok", "user-2"); err != ErrUnauthorized {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
}

func TestUserImageURL_CarriesAPIKeyAndUsesPublicBaseURL(t *testing.T) {
	c := NewClient("http://internal-only:8096")
	c.SetPublicBaseURL("https://public.example.com")

	got := c.UserImageURL("user-2", "tok-xyz")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "public.example.com" {
		t.Errorf("host = %q, want the public base URL, not the internal one", u.Host)
	}
	if u.Query().Get("api_key") != "tok-xyz" {
		t.Errorf("api_key = %q, want the requesting user's own token", u.Query().Get("api_key"))
	}
	if u.Path != "/Users/user-2/Images/Primary" {
		t.Errorf("path = %q", u.Path)
	}
}
