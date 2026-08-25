package config

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// configTemplateSrc is a text/template (not html/template -- the output is
// JSONC, not HTML) that renders a FileConfig into the commented style
// documented in README.md's "Config file (config.jsonc)" section. Embedded
// rather than built with encoding/json specifically so the written file
// keeps the explanatory comments -- see ARCHITECTURE.md §16's note that a
// future writer would need to be template-based for exactly this reason.
//
//go:embed templates/config.jsonc.tmpl
var configTemplateSrc string

// configTemplateFuncs supplies jsonString, which renders a Go string as a
// properly quoted-and-escaped JSON string value (via encoding/json, not
// hand-rolled escaping) -- required because operator-submitted text (a
// title, an origin, a URL) can contain characters like `"` or `\` that
// would otherwise corrupt the JSON if interpolated verbatim.
var configTemplateFuncs = template.FuncMap{
	"jsonString": func(s string) (string, error) {
		b, err := json.Marshal(s)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
}

var configTemplate = template.Must(template.New("config.jsonc").Funcs(configTemplateFuncs).Parse(configTemplateSrc))

// configTemplateData holds the plain (dereferenced, already-validated)
// values the template executes against -- deliberately not FileConfig
// itself, whose pointer fields exist for the load-time precedence merge
// (see FileConfig's doc comment), not for template rendering.
type configTemplateData struct {
	Title                 string
	LogLevel              string
	BrowserOrigins        []string
	ListenAddress         string
	SessionIdleTimeout    string
	SessionAgeTimeout     string
	HostGracePeriod       string
	InactivityTimeout     string
	ProgressInterval      string
	SyncSnapshotInterval  string
	SyncSoftDrift         string
	SyncHardDrift         string
	SyncMaxRateAdjustment float64
	ServerURL             string
	PublicURL             string
}

// WriteFile validates every field of fc -- using the same exported
// validators internal/config's own loader uses (see config.go), so there
// is exactly one implementation of each rule, not a second one that could
// drift -- and, only if every field passes, atomically writes fc to path
// as commented JSONC. No bytes are written to path unless validation and
// rendering both succeed in full: WriteFile renders into an in-memory
// buffer first, then writes that buffer to a temp file in the same
// directory as path and renames it into place, so a crash or interruption
// mid-write can never leave a partially-written or corrupted file at path,
// and a validation failure never touches path at all.
//
// fc must have every field populated (a nil pointer is treated as
// "missing" and rejected) except MediaServerSettings.PublicURL, which is
// optional. This is deliberately stricter than FileConfig's own load-time
// contract (where a nil pointer just means "fall through to env/default") --
// the wizard always collects every field it writes, so an unexpectedly nil
// pointer here indicates a bug in the caller, not a legitimate partial
// config.
//
// fc must never have a value for the token encryption key -- and, by
// construction, it can't: FileConfig has no field for one (see its own
// doc comment). WriteFile never accepts that value as a separate
// parameter either, so there is no code path through this function that
// could ever write it to disk.
func WriteFile(path string, fc *FileConfig) error {
	data, err := renderData(fc)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	if err := configTemplate.Execute(&buf, data); err != nil {
		return fmt.Errorf("rendering config.jsonc: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating config directory %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".config.jsonc.tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup: after a successful Rename below, tmpPath no
	// longer exists, so this Remove is a harmless no-op; it only matters
	// on an early-return error path, where it prevents leaving a stray
	// temp file behind.
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return fmt.Errorf("setting temp file permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming into place at %q: %w", path, err)
	}
	return nil
}

// renderData validates every field of fc via the same functions the
// loader (config.go) uses and returns the plain-valued struct the
// template executes against. See WriteFile's doc comment for the
// required-unless-PublicURL contract.
func renderData(fc *FileConfig) (*configTemplateData, error) {
	if fc == nil {
		return nil, fmt.Errorf("config: nil FileConfig")
	}

	d := &configTemplateData{}
	var err error

	if d.Title, err = requireString(fc.ServerSettings.Title, "server_settings.title"); err != nil {
		return nil, err
	}
	if err := ValidateTitle(d.Title); err != nil {
		return nil, fmt.Errorf("server_settings.title: %w", err)
	}

	if d.LogLevel, err = requireString(fc.ServerSettings.LogLevel, "server_settings.log_level"); err != nil {
		return nil, err
	}
	if err := ValidateLogLevel(d.LogLevel); err != nil {
		return nil, fmt.Errorf("server_settings.log_level: %w", err)
	}

	if fc.ServerSettings.BrowserOrigins == nil {
		return nil, fmt.Errorf("server_settings.browser_origins is required")
	}
	if d.BrowserOrigins, err = ValidateOrigins(*fc.ServerSettings.BrowserOrigins); err != nil {
		return nil, fmt.Errorf("server_settings.browser_origins: %w", err)
	}

	if d.ListenAddress, err = requireString(fc.ServerSettings.ListenAddress, "server_settings.listen_address"); err != nil {
		return nil, err
	}
	if err := ValidateListenAddress(d.ListenAddress); err != nil {
		return nil, fmt.Errorf("server_settings.listen_address: %w", err)
	}

	if d.SessionIdleTimeout, err = requireDuration(fc.ServerSettings.SessionIdleTimeout, "server_settings.session_idle_timeout"); err != nil {
		return nil, err
	}
	if d.SessionAgeTimeout, err = requireDuration(fc.ServerSettings.SessionAgeTimeout, "server_settings.session_age_timeout"); err != nil {
		return nil, err
	}
	if d.HostGracePeriod, err = requireDuration(fc.GlobalPartySettings.HostGracePeriod, "global_party_settings.host_grace_period"); err != nil {
		return nil, err
	}
	if d.InactivityTimeout, err = requireDuration(fc.GlobalPartySettings.InactivityTimeout, "global_party_settings.inactivity_timeout"); err != nil {
		return nil, err
	}
	if d.ProgressInterval, err = requireDuration(fc.GlobalPlaybackSettings.ProgressInterval, "global_playback_settings.progress_interval"); err != nil {
		return nil, err
	}
	if d.SyncSnapshotInterval, err = requireDuration(fc.GlobalPlaybackSettings.SyncSnapshotInterval, "global_playback_settings.sync_snapshot_interval"); err != nil {
		return nil, err
	}

	if d.SyncSoftDrift, err = requireStrictDuration(fc.GlobalPlaybackSettings.SyncSoftDrift, "global_playback_settings.sync_soft_drift"); err != nil {
		return nil, err
	}
	if d.SyncHardDrift, err = requireStrictDuration(fc.GlobalPlaybackSettings.SyncHardDrift, "global_playback_settings.sync_hard_drift"); err != nil {
		return nil, err
	}
	// Both already parsed successfully by requireStrictDuration above, so
	// the re-parse here can't fail.
	softD, _ := ParseDurationStrict(d.SyncSoftDrift)
	hardD, _ := ParseDurationStrict(d.SyncHardDrift)
	if err := ValidateSyncDrift(softD, hardD); err != nil {
		return nil, err
	}

	if fc.GlobalPlaybackSettings.SyncMaxRateAdjustment == nil {
		return nil, fmt.Errorf("global_playback_settings.sync_max_rate_adjustment is required")
	}
	d.SyncMaxRateAdjustment = *fc.GlobalPlaybackSettings.SyncMaxRateAdjustment
	if err := ValidateMaxRateAdjustment(d.SyncMaxRateAdjustment); err != nil {
		return nil, fmt.Errorf("global_playback_settings.sync_max_rate_adjustment: %w", err)
	}

	if d.ServerURL, err = requireString(fc.MediaServerSettings.ServerURL, "media_server_settings.server_url"); err != nil {
		return nil, err
	}

	if fc.MediaServerSettings.PublicURL != nil {
		d.PublicURL = *fc.MediaServerSettings.PublicURL
	}

	return d, nil
}

func requireString(p *string, field string) (string, error) {
	if p == nil || strings.TrimSpace(*p) == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	return *p, nil
}

// requireDuration validates p with the lenient parser (ParseDuration --
// bare integers accepted as seconds), matching every duration field
// except the two drift fields.
func requireDuration(p *string, field string) (string, error) {
	s, err := requireString(p, field)
	if err != nil {
		return "", err
	}
	if _, err := ParseDuration(s); err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	return s, nil
}

// requireStrictDuration validates p with ParseDurationStrict (explicit
// unit required, no bare-integer fallback), matching
// sync_soft_drift/sync_hard_drift specifically.
func requireStrictDuration(p *string, field string) (string, error) {
	s, err := requireString(p, field)
	if err != nil {
		return "", err
	}
	if _, err := ParseDurationStrict(s); err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	return s, nil
}
