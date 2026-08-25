package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// DefaultConfigPath is the fixed location of the JSONC config file. Its
// presence or absence is the sole signal for which startup mode the server
// runs in (setup-required vs. normal) -- there is no separate
// "enable_setup_wizard" field anywhere.
const DefaultConfigPath = "/data/config/config.jsonc"

// FileConfig mirrors config.jsonc's on-disk JSON shape. Every field is a
// pointer so a key that's absent from the file is distinguishable from one
// present with an explicit zero value -- both the env/file/default
// precedence merge (see config.go) and a future templated writer depend on
// that distinction: a writer only wants to round-trip fields the wizard
// actually collected, not silently invent zero values for everything else.
//
// Deliberately has no field for the encryption key, or any other secret --
// see CLAUDE.md's "no secrets in config.jsonc" invariant. Don't add one;
// TOKEN_ENCRYPTION_KEY / TOKEN_ENCRYPTION_KEY_FILE are resolved from the
// environment only, in tokenkey.go.
type FileConfig struct {
	ServerSettings struct {
		Title              *string   `json:"title"`
		LogLevel           *string   `json:"log_level"`
		BrowserOrigins     *[]string `json:"browser_origins"`
		ListenAddress      *string   `json:"listen_address"`
		SessionIdleTimeout *string   `json:"session_idle_timeout"`
		SessionAgeTimeout  *string   `json:"session_age_timeout"`
	} `json:"server_settings"`
	GlobalPartySettings struct {
		HostGracePeriod   *string `json:"host_grace_period"`
		InactivityTimeout *string `json:"inactivity_timeout"`
	} `json:"global_party_settings"`
	GlobalPlaybackSettings struct {
		ProgressInterval      *string  `json:"progress_interval"`
		SyncSnapshotInterval  *string  `json:"sync_snapshot_interval"`
		SyncSoftDrift         *string  `json:"sync_soft_drift"`
		SyncHardDrift         *string  `json:"sync_hard_drift"`
		SyncMaxRateAdjustment *float64 `json:"sync_max_rate_adjustment"`
	} `json:"global_playback_settings"`
	MediaServerSettings struct {
		ServerURL *string `json:"server_url"`
		PublicURL *string `json:"public_url"`
	} `json:"media_server_settings"`
}

// FileExists reports whether path exists and is readable-as-a-stat. Any
// os.Stat error other than "does not exist" (e.g. a permissions problem on
// a file that does exist) is surfaced as an error rather than silently
// treated as "not set up" -- a permission problem on an existing file is a
// misconfiguration, not an unconfigured server.
func FileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("checking for config file %q: %w", path, err)
}

// loadFile reads, comment-strips, and JSON-decodes the config file at path.
// Unknown fields (at any nesting level) are rejected rather than silently
// ignored -- an unrecognized key in a wizard-written file is far more
// likely a version-skew or operator bug than an intentional extension, and
// this project already prefers loud failure over silent tolerance
// elsewhere (e.g. SYNC_HARD_DRIFT_MS <= SYNC_SOFT_DRIFT_MS).
func loadFile(path string) (*FileConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	stripped := stripJSONC(raw)

	dec := json.NewDecoder(bytes.NewReader(stripped))
	dec.DisallowUnknownFields()

	var fc FileConfig
	if err := dec.Decode(&fc); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	return &fc, nil
}
