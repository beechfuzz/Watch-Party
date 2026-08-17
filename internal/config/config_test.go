package config

import (
	"testing"
	"time"
)

// setRequiredEnv sets the minimum env vars Load needs to succeed, using
// t.Setenv so each test gets an isolated, auto-restored environment.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("APP_ORIGINS", "https://watchparty.example.com")
	t.Setenv("EMBY_SERVER_URL", "https://emby.example.com")
	t.Setenv("TOKEN_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=") // 32 zero bytes, base64
}

func TestLoad_EmbyPublicURL_DefaultsToEmbyServerURL(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbyPublicURL != "" {
		t.Errorf("EmbyPublicURL = %q, want empty when EMBY_PUBLIC_URL is unset (caller defaults it via emby.Client)", cfg.EmbyPublicURL)
	}
}

func TestLoad_EmbyPublicURL_Override(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("EMBY_PUBLIC_URL", "https://emby.example.com/")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbyPublicURL != "https://emby.example.com" {
		t.Errorf("EmbyPublicURL = %q, want trailing slash trimmed", cfg.EmbyPublicURL)
	}
}

func TestLoad_PartyInactivityTimeout_DefaultsTo48Hours(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PartyInactivityTimeout != 48*time.Hour {
		t.Errorf("PartyInactivityTimeout = %v, want 48h", cfg.PartyInactivityTimeout)
	}
}

func TestLoad_PartyInactivityTimeout_Override(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PARTY_INACTIVITY_TIMEOUT", "12h")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PartyInactivityTimeout != 12*time.Hour {
		t.Errorf("PartyInactivityTimeout = %v, want 12h", cfg.PartyInactivityTimeout)
	}
}

func TestLoad_PUIDPGID_DefaultTo65532(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PUID != 65532 || cfg.PGID != 65532 {
		t.Errorf("PUID=%d PGID=%d, want 65532/65532 by default", cfg.PUID, cfg.PGID)
	}
}

func TestLoad_PUIDPGID_Override(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PUID", "1000")
	t.Setenv("PGID", "1001")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PUID != 1000 || cfg.PGID != 1001 {
		t.Errorf("PUID=%d PGID=%d, want 1000/1001", cfg.PUID, cfg.PGID)
	}
}

func TestLoad_PUID_Negative_Rejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PUID", "-1")
	if _, err := Load(); err == nil {
		t.Error("expected an error for negative PUID")
	}
}

func TestLoad_PGID_Negative_Rejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PGID", "-5")
	if _, err := Load(); err == nil {
		t.Error("expected an error for negative PGID")
	}
}

func TestLoad_MixedSchemeAppOrigins_Allowed(t *testing.T) {
	// A single Watch Party instance can legitimately be reachable at an
	// external HTTPS domain and an internal-only HTTP LAN hostname at the
	// same time — this must not be rejected. Session cookie security is
	// determined per-request instead (see internal/session).
	t.Setenv("APP_ORIGINS", "https://watchparty.example.com,http://watchparty.home")
	t.Setenv("EMBY_SERVER_URL", "https://emby.example.com")
	t.Setenv("TOKEN_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.AppOrigins) != 2 {
		t.Fatalf("AppOrigins = %v, want 2 entries", cfg.AppOrigins)
	}
}

func TestConfig_NonHTTPSOrigins(t *testing.T) {
	c := &Config{AppOrigins: []string{"https://watchparty.example.com", "http://watchparty.home", "https://also-fine.example.com"}}
	got := c.NonHTTPSOrigins()
	if len(got) != 1 || got[0] != "http://watchparty.home" {
		t.Errorf("NonHTTPSOrigins() = %v, want [http://watchparty.home]", got)
	}
}

func TestConfig_NonHTTPSOrigins_AllHTTPS(t *testing.T) {
	c := &Config{AppOrigins: []string{"https://a.example.com", "https://b.example.com"}}
	if got := c.NonHTTPSOrigins(); len(got) != 0 {
		t.Errorf("NonHTTPSOrigins() = %v, want empty", got)
	}
}

func TestLoad_DatabasePath_DefaultsToDataVolumeMountPoint(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DatabasePath != "/data/watchparty.db" {
		t.Errorf("DatabasePath = %q, want /data/watchparty.db (matching the volume mount point documented in docker-compose.yml/watchparty.container)", cfg.DatabasePath)
	}
}
