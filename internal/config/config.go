// Package config loads Watch Party's runtime configuration from environment
// variables. There is no config file format — env vars only, per the project
// spec's "all config via environment variables" requirement.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
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
	ListenAddr string
	// AppOrigins is used for WebSocket Origin validation. Mixed schemes are
	// fine — e.g. an external https:// domain alongside an internal-only
	// http:// LAN hostname for the same instance — since session cookie
	// Secure-ness is now determined per-request, not from a single global
	// flag; see internal/session.isSecureRequest and ARCHITECTURE.md §2.
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
	// as a normal user during development.
	PUID int
	PGID int

	// Logging
	LogLevel string
}

// Load reads configuration from the environment. It returns an error for any
// missing required value or value that fails validation.
func Load() (*Config, error) {
	cfg := &Config{
		ListenAddr: getEnvDefault("LISTEN_ADDR", ":8080"),
		// Defaults to the container's conventional volume mount point (see
		// docker-compose.yml and watchparty.container, both of which mount
		// a persistent volume at /data) — local `go run` development
		// should set DATABASE_PATH to something writable, e.g.
		// ./data/watchparty.db, via .env.
		DatabasePath: getEnvDefault("DATABASE_PATH", "/data/watchparty.db"),
		LogLevel:     strings.ToLower(getEnvDefault("LOG_LEVEL", "info")),
	}

	origins := getEnvDefault("APP_ORIGINS", "")
	if origins == "" {
		return nil, fmt.Errorf("APP_ORIGINS is required (comma-separated list of allowed origins, e.g. https://watchparty.example.com)")
	}
	for _, o := range strings.Split(origins, ",") {
		o = strings.TrimSpace(o)
		if o == "" {
			continue
		}
		cfg.AppOrigins = append(cfg.AppOrigins, strings.TrimRight(o, "/"))
	}
	if len(cfg.AppOrigins) == 0 {
		return nil, fmt.Errorf("APP_ORIGINS must contain at least one origin")
	}

	cfg.EmbyServerURL = strings.TrimRight(os.Getenv("EMBY_SERVER_URL"), "/")
	if cfg.EmbyServerURL == "" {
		return nil, fmt.Errorf("EMBY_SERVER_URL is required")
	}
	cfg.EmbyPublicURL = strings.TrimRight(os.Getenv("EMBY_PUBLIC_URL"), "/")

	keyRaw := os.Getenv("TOKEN_ENCRYPTION_KEY")
	if keyRaw == "" {
		return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY is required (32-byte key, base64 or hex encoded; generate with: openssl rand -base64 32)")
	}
	key, err := decodeKey(keyRaw)
	if err != nil {
		return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY: %w", err)
	}
	cfg.TokenEncryptionKey = key

	if cfg.SessionIdleTimeout, err = getEnvDuration("SESSION_IDLE_TIMEOUT", 24*time.Hour); err != nil {
		return nil, err
	}
	if cfg.SessionMaxAge, err = getEnvDuration("SESSION_MAX_AGE", 30*24*time.Hour); err != nil {
		return nil, err
	}
	if cfg.HostGracePeriod, err = getEnvDuration("HOST_GRACE_PERIOD_SECONDS", 20*time.Second); err != nil {
		return nil, err
	}
	if cfg.PartyInactivityTimeout, err = getEnvDuration("PARTY_INACTIVITY_TIMEOUT", 48*time.Hour); err != nil {
		return nil, err
	}
	if cfg.SyncSnapshotInterval, err = getEnvDuration("SYNC_SNAPSHOT_INTERVAL", 4*time.Second); err != nil {
		return nil, err
	}
	if cfg.EmbyProgressInterval, err = getEnvDuration("EMBY_PROGRESS_INTERVAL", 10*time.Second); err != nil {
		return nil, err
	}

	cfg.PUID = getEnvInt("PUID", defaultContainerID)
	cfg.PGID = getEnvInt("PGID", defaultContainerID)
	if cfg.PUID < 0 {
		return nil, fmt.Errorf("PUID must not be negative, got %d", cfg.PUID)
	}
	if cfg.PGID < 0 {
		return nil, fmt.Errorf("PGID must not be negative, got %d", cfg.PGID)
	}

	cfg.SyncSoftDriftMS = getEnvInt("SYNC_SOFT_DRIFT_MS", 300)
	cfg.SyncHardDriftMS = getEnvInt("SYNC_HARD_DRIFT_MS", 1500)
	if cfg.SyncHardDriftMS <= cfg.SyncSoftDriftMS {
		return nil, fmt.Errorf("SYNC_HARD_DRIFT_MS (%d) must be greater than SYNC_SOFT_DRIFT_MS (%d)", cfg.SyncHardDriftMS, cfg.SyncSoftDriftMS)
	}

	cfg.SyncMaxRateAdjust, err = getEnvFloat("SYNC_MAX_RATE_ADJUSTMENT", 0.05)
	if err != nil {
		return nil, err
	}
	if cfg.SyncMaxRateAdjust <= 0 || cfg.SyncMaxRateAdjust >= 1 {
		return nil, fmt.Errorf("SYNC_MAX_RATE_ADJUSTMENT must be between 0 and 1 (exclusive), got %v", cfg.SyncMaxRateAdjust)
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

func getEnvFloat(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid float value %q: %w", key, v, err)
	}
	return f, nil
}

// getEnvDuration parses a duration from the environment. For backward-
// compatible clarity with the env var names in the spec (e.g.
// HOST_GRACE_PERIOD_SECONDS), a bare integer is interpreted as seconds; a
// Go duration string (e.g. "30m", "1h") is also accepted.
func getEnvDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration value %q (use e.g. \"30s\", \"24h\", or a bare integer for seconds): %w", key, v, err)
	}
	return d, nil
}
