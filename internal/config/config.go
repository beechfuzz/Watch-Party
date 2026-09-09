// Package config loads Watch Party's runtime configuration by layering two
// sources: an optional JSONC file at DefaultConfigPath, and environment
// variables, which override the corresponding file value field-by-field
// when set. Precedence is: environment variable, if set, wins; otherwise
// the file value, if present; otherwise a hardcoded default, if one
// exists. TOKEN_ENCRYPTION_KEY / TOKEN_ENCRYPTION_KEY_FILE are the one
// exception -- environment-only, never read from the file (see
// tokenkey.go) -- and, unlike every other field, resolved only on the
// normal-mode startup path (Load/loadFrom), not when the server is
// running in setup-required mode (see FileExists and
// cmd/server/main.go's runSetupRequired).
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// defaultContainerID is the UID/GID PUID/PGID fall back to when unset. It
// matches the distroless "nonroot" convention this image used before
// PUID/PGID existed, so upgrading without setting either variable doesn't
// change file ownership underneath existing deployments.
const defaultContainerID = 65532

// Config holds all runtime configuration for the Watch Party server.
type Config struct {
	// Server
	Title      string
	ListenAddr string
	// AppOrigins is used for WebSocket Origin validation. Mixed schemes are
	// fine — e.g. an external https:// domain alongside an internal-only
	// http:// LAN hostname for the same instance — since session cookie
	// Secure-ness is now determined per-request, not from a single global
	// flag; see internal/session.IsSecureRequest and ARCHITECTURE.md §2.
	AppOrigins []string

	// Emby
	// EmbyServerURL is used for this server's own calls to Emby (auth,
	// PlaybackInfo, progress reporting) and, unless EmbyPublicURL is set,
	// also for playback URLs handed to the browser.
	EmbyServerURL string
	// EmbyPublicURL, if set, overrides the host used only for browser-facing
	// playback URLs (see emby.Client.SetPublicBaseURL). Needed when
	// EmbyServerURL points at an address only reachable from this server —
	// e.g. container-DNS on a shared Docker/Podman network — which a
	// browser on the user's LAN can't resolve; see ARCHITECTURE.md §5.2.
	EmbyPublicURL string

	// Persistence
	DatabasePath string

	// Security
	// TokenEncryptionKey is resolved only from TOKEN_ENCRYPTION_KEY /
	// TOKEN_ENCRYPTION_KEY_FILE — never from config.jsonc. See tokenkey.go.
	TokenEncryptionKey []byte // 32 bytes, AES-256-GCM key for encrypting Emby AccessTokens at rest

	// Session lifecycle
	SessionIdleTimeout time.Duration
	SessionMaxAge      time.Duration

	// Party lifecycle
	HostGracePeriod time.Duration
	// PartyInactivityTimeout is how long a party can go with no play/pause/
	// seek control and no member connecting/reconnecting before it's
	// automatically ended -- cleanup for a party left running (e.g. everyone
	// left without anyone clicking End Party, or the host's browser
	// crashed) rather than lingering forever. See ARCHITECTURE.md §8.
	PartyInactivityTimeout time.Duration

	// Sync tuning
	SyncSnapshotInterval time.Duration
	SyncSoftDriftMS      int
	SyncHardDriftMS      int
	SyncMaxRateAdjust    float64 // e.g. 0.05 == playback rate may be nudged to at most 1.05 / at least 0.95

	// Emby playback reporting
	EmbyProgressInterval time.Duration

	// Container UID/GID (the common self-hosted-image PUID/PGID pattern).
	// Only meaningful when the process is started as root (see Dockerfile
	// and internal/privdrop): in that case the server takes ownership of
	// its data directory and permanently drops to this UID/GID before
	// doing anything else. Has no effect otherwise — e.g. local `go run`
	// as a normal user during development. Env-var only: not represented
	// in config.jsonc, since it's a container-identity concern rather than
	// an app setting a wizard would collect.
	PUID int
	PGID int

	// Logging
	LogLevel string
}

// Load reads configuration by layering config.jsonc (if DefaultConfigPath
// exists) under environment variables, and returns an error for any
// missing required value or value that fails validation, whichever source
// it came from. Load is the normal-mode entrypoint: it also resolves and
// validates TOKEN_ENCRYPTION_KEY / TOKEN_ENCRYPTION_KEY_FILE (see
// tokenkey.go), which is why it must not be called from setup-required
// mode -- use ResolveListenAddress / ResolveLogLevel directly there
// instead (see setupmode.go).
func Load() (*Config, error) {
	return LoadFromPath(DefaultConfigPath)
}

// LoadFromPath is Load with the config file path made an explicit argument
// instead of the hardcoded DefaultConfigPath. Load is just
// LoadFromPath(DefaultConfigPath); this exists so callers that need to
// point at a different path -- today, only tests driving the full
// setup-required -> normal mode transition against a real temporary
// config.jsonc (see cmd/server/run_test.go) -- don't need to touch
// DefaultConfigPath itself, which stays the single hardcoded production
// path everywhere else.
func LoadFromPath(path string) (*Config, error) {
	exists, err := FileExists(path)
	if err != nil {
		return nil, err
	}

	var fc *FileConfig
	if exists {
		fc, err = loadFile(path)
		if err != nil {
			return nil, err
		}
	}

	return loadFrom(fc)
}

// loadFrom merges fc (nil if config.jsonc doesn't exist -- every field then
// falls through to its environment variable or hardcoded default, matching
// today's env-only behavior exactly) with environment variable overrides,
// validating every field regardless of which source produced it.
func loadFrom(fc *FileConfig) (*Config, error) {
	if fc == nil {
		fc = &FileConfig{}
	}

	cfg := &Config{}
	var err error

	cfg.Title = resolveString("SERVER_TITLE", fc.ServerSettings.Title, "Watch Party")
	if err := ValidateTitle(cfg.Title); err != nil {
		return nil, fmt.Errorf("server_settings.title / SERVER_TITLE %w", err)
	}

	if cfg.LogLevel, err = ResolveLogLevel(fc.ServerSettings.LogLevel); err != nil {
		return nil, err
	}

	if cfg.AppOrigins, err = resolveOrigins("APP_ORIGINS", fc.ServerSettings.BrowserOrigins); err != nil {
		return nil, err
	}

	if cfg.ListenAddr, err = ResolveListenAddress(fc.ServerSettings.ListenAddress); err != nil {
		return nil, err
	}

	if cfg.SessionIdleTimeout, err = resolveDuration("SESSION_IDLE_TIMEOUT", "server_settings.session_idle_timeout", fc.ServerSettings.SessionIdleTimeout, 24*time.Hour); err != nil {
		return nil, err
	}
	if err := ValidateDisableableDuration("session_idle_timeout", cfg.SessionIdleTimeout); err != nil {
		return nil, err
	}
	if cfg.SessionMaxAge, err = resolveDuration("SESSION_MAX_AGE", "server_settings.session_age_timeout", fc.ServerSettings.SessionAgeTimeout, 30*24*time.Hour); err != nil {
		return nil, err
	}
	if err := ValidateDisableableDuration("session_age_timeout", cfg.SessionMaxAge); err != nil {
		return nil, err
	}

	if cfg.HostGracePeriod, err = resolveDuration("HOST_GRACE_PERIOD_SECONDS", "global_party_settings.host_grace_period", fc.GlobalPartySettings.HostGracePeriod, 20*time.Second); err != nil {
		return nil, err
	}
	if cfg.PartyInactivityTimeout, err = resolveDuration("PARTY_INACTIVITY_TIMEOUT", "global_party_settings.inactivity_timeout", fc.GlobalPartySettings.InactivityTimeout, 48*time.Hour); err != nil {
		return nil, err
	}

	if cfg.EmbyProgressInterval, err = resolveDuration("EMBY_PROGRESS_INTERVAL", "global_playback_settings.progress_interval", fc.GlobalPlaybackSettings.ProgressInterval, 10*time.Second); err != nil {
		return nil, err
	}
	if err := ValidateDisableableDuration("progress_interval", cfg.EmbyProgressInterval); err != nil {
		return nil, err
	}
	if cfg.SyncSnapshotInterval, err = resolveDuration("SYNC_SNAPSHOT_INTERVAL", "global_playback_settings.sync_snapshot_interval", fc.GlobalPlaybackSettings.SyncSnapshotInterval, 4*time.Second); err != nil {
		return nil, err
	}
	if err := ValidateDisableableDuration("sync_snapshot_interval", cfg.SyncSnapshotInterval); err != nil {
		return nil, err
	}

	if cfg.SyncSoftDriftMS, err = resolveDriftMS("SYNC_SOFT_DRIFT_MS", "global_playback_settings.sync_soft_drift", fc.GlobalPlaybackSettings.SyncSoftDrift, 300); err != nil {
		return nil, err
	}
	if err := ValidateDisableableDuration("sync_soft_drift", time.Duration(cfg.SyncSoftDriftMS)*time.Millisecond); err != nil {
		return nil, err
	}
	if cfg.SyncHardDriftMS, err = resolveDriftMS("SYNC_HARD_DRIFT_MS", "global_playback_settings.sync_hard_drift", fc.GlobalPlaybackSettings.SyncHardDrift, 1500); err != nil {
		return nil, err
	}
	if err := ValidateDisableableDuration("sync_hard_drift", time.Duration(cfg.SyncHardDriftMS)*time.Millisecond); err != nil {
		return nil, err
	}
	if err := ValidateSyncDrift(time.Duration(cfg.SyncSoftDriftMS)*time.Millisecond, time.Duration(cfg.SyncHardDriftMS)*time.Millisecond); err != nil {
		return nil, err
	}

	if cfg.SyncMaxRateAdjust, err = resolveFloat("SYNC_MAX_RATE_ADJUSTMENT", fc.GlobalPlaybackSettings.SyncMaxRateAdjustment, 0.05); err != nil {
		return nil, err
	}
	if err := ValidateMaxRateAdjustment(cfg.SyncMaxRateAdjust); err != nil {
		return nil, err
	}

	if cfg.EmbyServerURL, err = resolveStringRequired("EMBY_SERVER_URL", fc.MediaServerSettings.ServerURL, "EMBY_SERVER_URL or media_server_settings.server_url is required"); err != nil {
		return nil, err
	}
	cfg.EmbyServerURL = strings.TrimRight(cfg.EmbyServerURL, "/")

	cfg.EmbyPublicURL = strings.TrimRight(resolveString("EMBY_PUBLIC_URL", fc.MediaServerSettings.PublicURL, ""), "/")

	// Env-only fields: no config.jsonc representation (see CLAUDE.md /
	// ARCHITECTURE.md for why -- container identity and filesystem path,
	// not app settings a wizard would collect). Unchanged from before the
	// file layer existed.
	cfg.DatabasePath = getEnvDefault("DATABASE_PATH", "/data/watchparty.db")

	cfg.PUID = getEnvInt("PUID", defaultContainerID)
	cfg.PGID = getEnvInt("PGID", defaultContainerID)
	if cfg.PUID < 0 {
		return nil, fmt.Errorf("PUID must not be negative, got %d", cfg.PUID)
	}
	if cfg.PGID < 0 {
		return nil, fmt.Errorf("PGID must not be negative, got %d", cfg.PGID)
	}

	if cfg.TokenEncryptionKey, err = resolveTokenEncryptionKey(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// NonHTTPSOrigins returns every configured origin that isn't https://, for
// callers that want to log a startup reminder — see main.go. This is
// informational only: it's a legitimate, supported configuration (e.g. an
// internal-only LAN hostname alongside an external HTTPS domain), not a
// warning-worthy misconfiguration by itself.
func (c *Config) NonHTTPSOrigins() []string {
	var out []string
	for _, o := range c.AppOrigins {
		if !strings.HasPrefix(o, "https://") {
			out = append(out, o)
		}
	}
	return out
}

// decodeKey accepts either a base64 (standard, with or without padding) or
// hex encoded 32-byte key, since both are common ways operators generate
// secrets (e.g. `openssl rand -base64 32` vs `openssl rand -hex 32`).
func decodeKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.RawStdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, fmt.Errorf("must decode to exactly 32 bytes as base64 or hex (got %d raw chars)", len(raw))
}

// GenerateKey returns a fresh random 32-byte key, base64-encoded. Exposed for
// a `--generate-key` CLI convenience flag so operators don't need openssl.
func GenerateKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func getEnvDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getEnvInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// resolveString implements the env/file/default precedence chain for a
// plain string field. An empty string from either env or file is treated
// as "not set" (falls through to the next source), matching this
// project's pre-existing env-var convention (see the old getEnvDefault).
func resolveString(envKey string, fileVal *string, def string) string {
	if v, ok := os.LookupEnv(envKey); ok && v != "" {
		return v
	}
	if fileVal != nil && *fileVal != "" {
		return *fileVal
	}
	return def
}

// resolveStringRequired is resolveString without a default: if neither
// source provides a non-empty value, it returns errMsg as the error.
func resolveStringRequired(envKey string, fileVal *string, errMsg string) (string, error) {
	if v, ok := os.LookupEnv(envKey); ok && v != "" {
		return v, nil
	}
	if fileVal != nil && *fileVal != "" {
		return *fileVal, nil
	}
	return "", fmt.Errorf("%s", errMsg)
}

// resolveOrigins implements the env/file/default precedence chain for
// AppOrigins specifically, since the two sources use different native
// formats: the env var is comma-separated (unchanged from before the file
// layer existed), the file value is a native JSON array. The actual
// validation (trim, drop empties, require at least one) is delegated to
// ValidateOrigins so there is exactly one implementation of it -- see that
// function's doc comment for why it's exported.
func resolveOrigins(envKey string, fileVal *[]string) ([]string, error) {
	if v, ok := os.LookupEnv(envKey); ok && v != "" {
		origins, err := ValidateOrigins(strings.Split(v, ","))
		if err != nil {
			return nil, fmt.Errorf("%s %w", envKey, err)
		}
		return origins, nil
	}
	if fileVal != nil && len(*fileVal) > 0 {
		origins, err := ValidateOrigins(*fileVal)
		if err != nil {
			return nil, fmt.Errorf("server_settings.browser_origins %w", err)
		}
		return origins, nil
	}
	return nil, fmt.Errorf("APP_ORIGINS (comma-separated) or server_settings.browser_origins (JSON array) is required -- at least one allowed origin, e.g. https://watchparty.example.com")
}

// ValidateOrigins trims whitespace and a trailing slash from each of raw,
// drops empty entries, and requires at least one origin survive. Exported
// so the setup wizard can validate an operator-submitted browser_origins
// value with exactly the same rule resolveOrigins already applies to both
// the env var and file sources -- deliberately no stricter than that (e.g.
// no URL-scheme validation), since the existing loader doesn't require
// well-formed origins either and the wizard shouldn't silently diverge by
// being pickier than the loader it's writing input for.
func ValidateOrigins(raw []string) ([]string, error) {
	var origins []string
	for _, o := range raw {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		origins = append(origins, strings.TrimRight(o, "/"))
	}
	if len(origins) == 0 {
		return nil, fmt.Errorf("must contain at least one non-empty origin")
	}
	return origins, nil
}

// resolveDuration implements the env/file/default precedence chain for a
// duration field. Both sources accept the same format: a bare integer
// (seconds) or a Go duration string (e.g. "30s", "24h") -- see
// ParseDuration. fileField is a dotted path used only in the file-sourced
// error message, so a bad value's origin is unambiguous in the error.
func resolveDuration(envKey, fileField string, fileVal *string, def time.Duration) (time.Duration, error) {
	if v, ok := os.LookupEnv(envKey); ok && v != "" {
		d, err := ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", envKey, err)
		}
		return d, nil
	}
	if fileVal != nil && *fileVal != "" {
		d, err := ParseDuration(*fileVal)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", fileField, err)
		}
		return d, nil
	}
	return def, nil
}

// ParseDuration parses a duration from either an env var or a config.jsonc
// string value. For backward-compatible clarity with the env var names in
// the spec (e.g. HOST_GRACE_PERIOD_SECONDS), a bare integer is interpreted
// as seconds; a Go duration string (e.g. "30m", "1h") is also accepted.
// This is the general-purpose parser used by every duration field except
// sync_soft_drift/sync_hard_drift, which use the stricter
// ParseDurationStrict (see resolveDriftMS) precisely because the bare-
// integer-means-seconds convention here would be actively wrong for a
// millisecond-scale field.
func ParseDuration(raw string) (time.Duration, error) {
	if n, err := strconv.Atoi(raw); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid duration value %q (use e.g. \"30s\", \"24h\", or a bare integer for seconds): %w", raw, err)
	}
	return d, nil
}

// ParseDurationStrict parses a duration string with no bare-integer
// fallback -- an explicit unit is always required. Used only for
// sync_soft_drift/sync_hard_drift's config.jsonc values (see resolveDriftMS):
// those are millisecond-scale fields, so silently treating a unit-less
// "300" as "300 seconds" (the convention every other duration field in
// this config uses) would turn a sub-second drift threshold into a
// nonsensical five-minute one.
func ParseDurationStrict(raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid duration value %q (an explicit unit is required here, e.g. \"300ms\" -- a bare number is rejected to avoid ambiguity with the bare-integer-means-seconds convention used elsewhere in this config): %w", raw, err)
	}
	return d, nil
}

// resolveDriftMS implements the env/file/default precedence chain for
// SyncSoftDriftMS/SyncHardDriftMS. The two sources deliberately use
// different formats: the env var (existing, unchanged) is a bare integer
// number of milliseconds; the file value (new) is a duration string with
// an explicit unit, parsed via ParseDurationStrict. fileField is a dotted
// path used only in the file-sourced error message.
func resolveDriftMS(envKey, fileField string, fileVal *string, def int) (int, error) {
	if v, ok := os.LookupEnv(envKey); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("%s: invalid integer value %q: %w", envKey, v, err)
		}
		return n, nil
	}
	if fileVal != nil && *fileVal != "" {
		d, err := ParseDurationStrict(*fileVal)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", fileField, err)
		}
		return int(d.Milliseconds()), nil
	}
	return def, nil
}

// resolveFloat implements the env/file/default precedence chain for a
// float field. The file value is already a native JSON number (no text
// parsing needed); only the env var needs parsing.
func resolveFloat(envKey string, fileVal *float64, def float64) (float64, error) {
	if v, ok := os.LookupEnv(envKey); ok && v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: invalid float value %q: %w", envKey, v, err)
		}
		return f, nil
	}
	if fileVal != nil {
		return *fileVal, nil
	}
	return def, nil
}

// ValidateListenAddress checks that addr parses as a usable host:port (or
// bare ":port") address -- e.g. ":8080" or "0.0.0.0:8080". Previously any
// string was accepted verbatim into http.Server.Addr, surfacing a
// malformed value only as a runtime ListenAndServe error; this validates
// it at config-load time instead, so it's caught alongside every other
// config error.
func ValidateListenAddress(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("invalid listen address %q: missing port", addr)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 {
		return fmt.Errorf("invalid listen address %q: port must be a number between 0 and 65535", addr)
	}
	return nil
}

// ValidateLogLevel checks level against the set logging.New recognizes.
// Previously any unrecognized value silently fell back to "info"; this
// makes an invalid value a hard config error instead.
func ValidateLogLevel(level string) error {
	switch level {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("invalid log level %q: must be one of debug, info, warn, error", level)
	}
}

// ValidateTitle rejects a blank (or all-whitespace) title. Exported --
// alongside ValidateSyncDrift/ValidateMaxRateAdjustment below -- so the
// setup wizard can run exactly this check on an operator-submitted title
// instead of re-implementing "must not be blank" a second time; loadFrom
// calls it too, so there's exactly one implementation either way.
func ValidateTitle(title string) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("must not be blank")
	}
	return nil
}

// ValidateSyncDrift enforces that hard is strictly greater than soft --
// otherwise a "hard" drift correction (a seek) could trigger at or before
// the "soft" one (a rate nudge) does, which makes no sense. Extracted out
// of loadFrom (which calls this too) specifically so the setup wizard can
// validate an operator's submitted sync_soft_drift/sync_hard_drift pair
// with the exact same rule the loader enforces, rather than a second,
// possibly-drifting reimplementation of ">".
//
// A negative value disables the corresponding correction entirely (see
// ValidateDisableableDuration) -- ordering between "disabled" and any other
// value, including another disabled value, isn't a meaningful comparison,
// so the ordering check is skipped whenever either side is negative. Without
// this exemption, disabling both (e.g. both -1s) would fail hard<=soft even
// though "disable both drift corrections" is a legitimate configuration.
func ValidateSyncDrift(soft, hard time.Duration) error {
	if soft < 0 || hard < 0 {
		return nil
	}
	if hard <= soft {
		return fmt.Errorf("sync_hard_drift (%s) must be greater than sync_soft_drift (%s)", hard, soft)
	}
	return nil
}

// ValidateDisableableDuration rejects exactly zero for a duration field
// that supports "disabled" via a negative value instead. Zero is never a
// safe or meaningful value for these fields: fed into time.NewTicker it
// panics outright (internal/party's snapshot ticker,
// internal/embyreport's progress ticker); fed into a plain comparison it
// silently inverts the field's intended meaning (e.g. internal/syncalg's
// drift comparisons would fire on every nonzero drift, and a session idle
// timeout of zero would expire every session on its very next request,
// rather than disabling anything). A negative value is accepted here and
// left for the consuming code to treat as "disabled" -- only the literal,
// dangerous zero is rejected at load time.
func ValidateDisableableDuration(fieldName string, d time.Duration) error {
	if d == 0 {
		return fmt.Errorf("%s must not be exactly 0 -- use a negative value (e.g. \"-1s\") to disable this feature, or a positive duration to set it", fieldName)
	}
	return nil
}

// ValidateMaxRateAdjustment enforces 0 < v < 1 -- a playback rate nudge of
// 0 would never correct drift, and one >= 1 could reverse or freeze
// playback. Extracted out of loadFrom (which calls this too) for the same
// single-implementation reason as ValidateSyncDrift.
func ValidateMaxRateAdjustment(v float64) error {
	if v <= 0 || v >= 1 {
		return fmt.Errorf("sync_max_rate_adjustment must be between 0 and 1 (exclusive), got %v", v)
	}
	return nil
}
