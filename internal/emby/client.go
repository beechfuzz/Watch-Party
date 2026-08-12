// Package emby is a thin client for the subset of the Emby HTTP API Watch
// Party needs: authenticating a user, fetching item metadata, constructing
// playback URLs, and per-user playback progress reporting. Endpoint shapes
// come from the Phase 0 investigation recorded in ARCHITECTURE.md — treat
// anything here as unverified against a live Emby server until confirmed.
//
// Every call here uses the requesting user's own AccessToken; nothing in
// this package accepts or uses a shared service-account token, per the
// spec's hard requirement.
package emby

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const appName = "Watch Party"

// AppVersion is embedded in the X-Emby-Authorization header. Overridable at
// build time via -ldflags if the project ever adds real version numbering;
// a fixed placeholder is fine for a self-hosted tool with no release train.
var AppVersion = "0.1.0"

// ErrUnauthorized is returned when Emby rejects the AccessToken (401).
// Callers (the session layer) use this to force re-authentication rather
// than retrying a dead token indefinitely, per the spec's requirement.
var ErrUnauthorized = fmt.Errorf("emby: unauthorized (token rejected)")

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// DeviceID derives a stable, deterministic per-user device identifier for
// Emby's device-tracking features. It's stable across logins (so Emby
// doesn't accumulate a new "device" entry every time the same user signs
// in) without needing to persist anything extra — derived from the
// username via SHA-256 rather than used raw, purely to avoid putting a
// user-chosen string directly into an HTTP header value.
func DeviceID(username string) string {
	sum := sha256.Sum256([]byte("watchparty:" + username))
	return "watchparty-" + hex.EncodeToString(sum[:])[:16]
}

func embyAuthHeader(deviceID string) string {
	return fmt.Sprintf(`Emby Client="%s", Device="%s", DeviceId="%s", Version="%s"`,
		appName, appName, deviceID, AppVersion)
}

// AuthResult is the subset of Emby's AuthenticateByName response Watch
// Party needs.
type AuthResult struct {
	AccessToken string
	UserID      string
	DisplayName string
}

// AuthenticateByName proxies credentials straight to Emby's
// POST /Users/AuthenticateByName. The caller is responsible for discarding
// the plaintext password immediately after this returns (see the auth
// HTTP handler) — this function itself never stores it.
func (c *Client) AuthenticateByName(ctx context.Context, username, password string) (*AuthResult, error) {
	body, err := json.Marshal(map[string]string{"Username": username, "Pw": password})
	if err != nil {
		return nil, fmt.Errorf("emby: marshal auth request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/Users/AuthenticateByName", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("emby: build auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Authorization", embyAuthHeader(DeviceID(username)))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("emby: auth request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("emby: auth failed with status %d", resp.StatusCode)
	}

	var parsed struct {
		AccessToken string `json:"AccessToken"`
		User        struct {
			ID   string `json:"Id"`
			Name string `json:"Name"`
		} `json:"User"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("emby: decode auth response: %w", err)
	}
	if parsed.AccessToken == "" || parsed.User.ID == "" {
		return nil, fmt.Errorf("emby: auth response missing AccessToken or User.Id")
	}
	return &AuthResult{AccessToken: parsed.AccessToken, UserID: parsed.User.ID, DisplayName: parsed.User.Name}, nil
}

// ItemInfo is the subset of Emby's item metadata Watch Party needs.
type ItemInfo struct {
	ID           string
	Name         string
	RunTimeTicks int64
}

// GetItem fetches metadata for a media item, used at party creation to
// persist the authoritative duration_ticks.
func (c *Client) GetItem(ctx context.Context, accessToken, userID, itemID string) (*ItemInfo, error) {
	u := fmt.Sprintf("%s/Users/%s/Items/%s", c.baseURL, url.PathEscape(userID), url.PathEscape(itemID))
	var parsed struct {
		ID           string `json:"Id"`
		Name         string `json:"Name"`
		RunTimeTicks int64  `json:"RunTimeTicks"`
	}
	if err := c.getJSON(ctx, u, accessToken, &parsed); err != nil {
		return nil, err
	}
	return &ItemInfo{ID: parsed.ID, Name: parsed.Name, RunTimeTicks: parsed.RunTimeTicks}, nil
}

// MediaSource is the subset of Emby's MediaSourceInfo Watch Party needs to
// build a playback URL. There is deliberately no DirectStreamUrl field —
// per the Phase 0 findings, Emby does not return one; the client builds
// the direct-play/stream URL itself. TranscodingUrl, by contrast, IS
// returned by Emby for the transcode case (server-relative, missing only
// the host and api_key) and is preferred over hand-building one — see
// GetPlaybackURL.
type MediaSource struct {
	ID                   string `json:"Id"`
	Container            string `json:"Container"`
	SupportsDirectPlay   bool   `json:"SupportsDirectPlay"`
	SupportsDirectStream bool   `json:"SupportsDirectStream"`
	SupportsTranscoding  bool   `json:"SupportsTranscoding"`
	TranscodingUrl       string `json:"TranscodingUrl"`
}

type playbackInfoResponse struct {
	MediaSources  []MediaSource `json:"MediaSources"`
	PlaySessionId string        `json:"PlaySessionId"`
	ErrorCode     string        `json:"ErrorCode"`
}

// browserDeviceProfile tells Emby what a generic modern browser's <video>
// element can actually decode natively, so PlaybackInfo's
// SupportsDirectPlay/SupportsDirectStream reflect real browser capability
// instead of just "can the server serve these bytes as-is" — the two are
// very different questions (a browser cannot open an .mkv container at
// all, regardless of the codec inside, even though Emby is perfectly happy
// to serve one unmodified). Without this, Emby has no idea it's talking to
// a browser and defaults to permissive server-side answers, which is what
// produced "resource not suitable" errors in practice — see ARCHITECTURE.md.
//
// Deliberately conservative on the direct-play side (only combinations
// virtually every current browser supports natively) and routes everything
// else through HLS transcoding — a working transcode beats a direct-play
// claim that fails to actually load.
func browserDeviceProfile() map[string]any {
	return map[string]any{
		"MaxStreamingBitrate": 120_000_000,
		"DirectPlayProfiles": []map[string]any{
			{"Container": "mp4,m4v", "Type": "Video", "VideoCodec": "h264,vp9,av1", "AudioCodec": "aac,mp3,opus,flac"},
			{"Container": "webm", "Type": "Video", "VideoCodec": "vp8,vp9,av1", "AudioCodec": "opus,vorbis"},
		},
		"TranscodingProfiles": []map[string]any{
			{"Container": "ts", "Type": "Video", "VideoCodec": "h264", "AudioCodec": "aac", "Protocol": "hls", "Context": "Streaming"},
		},
		"SubtitleProfiles": []map[string]any{
			{"Format": "vtt", "Method": "External"},
		},
	}
}

// PlaybackURLResult is what the frontend needs to point a <video> element
// (or an HLS player) at Emby directly. JSON tags matter here: this is
// serialized straight to the browser (see httpapi.handlePlaybackURL) and
// must match what player.js actually reads.
type PlaybackURLResult struct {
	URL           string `json:"url"`
	IsTranscoded  bool   `json:"is_transcoded"`
	MediaSourceID string `json:"media_source_id"`
	PlaySessionID string `json:"play_session_id"`
	Container     string `json:"container"`
}

// GetPlaybackURL calls PlaybackInfo — POSTing a DeviceProfile describing
// what a browser can actually decode, so Emby's SupportsDirectPlay/
// SupportsDirectStream/SupportsTranscoding reflect real browser capability
// rather than raw server-side "can I serve this file" capability — and
// constructs the resulting stream URL. It always includes the requesting
// user's own AccessToken as the api_key query parameter, since a plain
// <video src> cannot set custom headers — see ARCHITECTURE.md §0.2.
func (c *Client) GetPlaybackURL(ctx context.Context, accessToken, userID, itemID, deviceID string) (*PlaybackURLResult, error) {
	reqBody, err := json.Marshal(map[string]any{
		"UserId":               userID,
		"DeviceProfile":        browserDeviceProfile(),
		"AutoOpenLiveStream":   false,
		"EnableDirectPlay":     true,
		"EnableDirectStream":   true,
		"EnableTranscoding":    true,
		"AllowVideoStreamCopy": true,
		"AllowAudioStreamCopy": true,
	})
	if err != nil {
		return nil, fmt.Errorf("emby: marshal playback info request: %w", err)
	}
	u := fmt.Sprintf("%s/Items/%s/PlaybackInfo", c.baseURL, url.PathEscape(itemID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("emby: build playback info request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", accessToken)

	var info playbackInfoResponse
	if err := c.do(req, &info); err != nil {
		return nil, err
	}
	if info.ErrorCode != "" {
		return nil, fmt.Errorf("emby: playback info error: %s", info.ErrorCode)
	}
	if len(info.MediaSources) == 0 {
		return nil, fmt.Errorf("emby: no media sources returned for item %s", itemID)
	}
	src := info.MediaSources[0]

	q := url.Values{}
	q.Set("MediaSourceId", src.ID)
	q.Set("PlaySessionId", info.PlaySessionId)
	q.Set("api_key", accessToken)

	if src.SupportsDirectStream || src.SupportsDirectPlay {
		container := src.Container
		if container == "" {
			container = "mp4"
		}
		q.Set("Static", "true")
		streamURL := fmt.Sprintf("%s/Videos/%s/stream.%s?%s", c.baseURL, url.PathEscape(itemID), container, q.Encode())
		return &PlaybackURLResult{
			URL: streamURL, IsTranscoded: false,
			MediaSourceID: src.ID, PlaySessionID: info.PlaySessionId, Container: container,
		}, nil
	}
	if src.SupportsTranscoding {
		q.Set("DeviceId", deviceID)
		hlsURL := src.TranscodingUrl
		if hlsURL != "" {
			// Emby-provided, server-relative, already carries
			// MediaSourceId/PlaySessionId/DeviceId — just needs the host
			// and our token appended.
			sep := "&"
			if !strings.Contains(hlsURL, "?") {
				sep = "?"
			}
			hlsURL = c.baseURL + hlsURL + sep + "api_key=" + url.QueryEscape(accessToken)
		} else {
			hlsURL = fmt.Sprintf("%s/Videos/%s/master.m3u8?%s", c.baseURL, url.PathEscape(itemID), q.Encode())
		}
		return &PlaybackURLResult{
			URL: hlsURL, IsTranscoded: true,
			MediaSourceID: src.ID, PlaySessionID: info.PlaySessionId,
		}, nil
	}
	return nil, fmt.Errorf("emby: media source %s supports neither direct play/stream nor transcoding", src.ID)
}

// --- per-user playback progress reporting (Sessions/Playing*) ---

// PlayingReport is the body shared by all three Sessions/Playing* calls.
// PositionTicks must always be server-derived — never taken verbatim from
// a client — per the spec's hard requirement that a browser cannot corrupt
// a user's real Emby watch history.
type PlayingReport struct {
	ItemID        string
	MediaSourceID string
	PositionTicks int64
	IsPaused      bool
	PlayMethod    string // "DirectPlay" | "DirectStream" | "Transcode"
	PlaySessionID string
	CanSeek       bool
}

func (r PlayingReport) toBody() map[string]any {
	return map[string]any{
		"ItemId":        r.ItemID,
		"MediaSourceId": r.MediaSourceID,
		"PositionTicks": r.PositionTicks,
		"IsPaused":      r.IsPaused,
		"PlayMethod":    r.PlayMethod,
		"PlaySessionId": r.PlaySessionID,
		"CanSeek":       r.CanSeek,
	}
}

func (c *Client) ReportPlaybackStart(ctx context.Context, accessToken, deviceID string, r PlayingReport) error {
	return c.postJSON(ctx, "/Sessions/Playing", accessToken, deviceID, r.toBody())
}

func (c *Client) ReportPlaybackProgress(ctx context.Context, accessToken, deviceID string, r PlayingReport) error {
	return c.postJSON(ctx, "/Sessions/Playing/Progress", accessToken, deviceID, r.toBody())
}

func (c *Client) ReportPlaybackStopped(ctx context.Context, accessToken, deviceID string, r PlayingReport) error {
	return c.postJSON(ctx, "/Sessions/Playing/Stopped", accessToken, deviceID, r.toBody())
}

// --- internal HTTP helpers ---
//
// Neither of these ever logs the URL or token: query strings here can
// carry api_key, and request/response bodies can carry AccessToken. Errors
// returned include only status codes and Emby's ErrorCode where present.

func (c *Client) getJSON(ctx context.Context, fullURL, accessToken string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return fmt.Errorf("emby: build request: %w", err)
	}
	req.Header.Set("X-Emby-Token", accessToken)
	return c.do(req, out)
}

func (c *Client) postJSON(ctx context.Context, path, accessToken, deviceID string, body map[string]any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("emby: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("emby: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", accessToken)
	if deviceID != "" {
		req.Header.Set("X-Emby-Authorization", embyAuthHeader(deviceID))
	}
	return c.do(req, nil)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("emby: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("emby: request failed with status %d", resp.StatusCode)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("emby: decode response: %w", err)
	}
	return nil
}
