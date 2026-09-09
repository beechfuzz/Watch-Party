package config

import (
	"strings"
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

// --- precedence matrix: env / file / default, per field type ---
//
// setRequiredFC returns a *FileConfig with every field the merge needs to
// succeed pre-populated with valid file values, then setRequiredEnvKey
// (below) additionally sets the required env vars -- so each test below
// only needs to touch the one field it's actually exercising.

func strp(s string) *string       { return &s }
func f64p(f float64) *float64     { return &f }
func sliceP(s []string) *[]string { return &s }

func fullFileConfig() *FileConfig {
	fc := &FileConfig{}
	fc.ServerSettings.Title = strp("File Title")
	fc.ServerSettings.LogLevel = strp("warn")
	fc.ServerSettings.BrowserOrigins = sliceP([]string{"https://file.example.com"})
	fc.ServerSettings.ListenAddress = strp(":7070")
	fc.ServerSettings.SessionIdleTimeout = strp("1h")
	fc.ServerSettings.SessionAgeTimeout = strp("2h")
	fc.GlobalPartySettings.HostGracePeriod = strp("10s")
	fc.GlobalPartySettings.InactivityTimeout = strp("3h")
	fc.GlobalPlaybackSettings.ProgressInterval = strp("6s")
	fc.GlobalPlaybackSettings.SyncSnapshotInterval = strp("7s")
	fc.GlobalPlaybackSettings.SyncSoftDrift = strp("250ms")
	fc.GlobalPlaybackSettings.SyncHardDrift = strp("2500ms")
	fc.GlobalPlaybackSettings.SyncMaxRateAdjustment = f64p(0.2)
	fc.MediaServerSettings.ServerURL = strp("https://file-emby.example.com")
	fc.MediaServerSettings.PublicURL = strp("https://file-emby-public.example.com")
	return fc
}

// setKeyEnv sets a valid TOKEN_ENCRYPTION_KEY -- loadFrom (unlike the
// setup-mode resolvers) always needs one, regardless of which round-2
// scenario is under test.
func setKeyEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TOKEN_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
}

func TestLoadFrom_NilFileConfig_MatchesEnvOnlyDefaults(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := loadFrom(nil)
	if err != nil {
		t.Fatalf("loadFrom(nil): %v", err)
	}
	if cfg.Title != "Watch Party" {
		t.Errorf("Title = %q, want default \"Watch Party\"", cfg.Title)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want default \":8080\"", cfg.ListenAddr)
	}
	if cfg.SyncSoftDriftMS != 300 || cfg.SyncHardDriftMS != 1500 {
		t.Errorf("drift = %d/%d, want defaults 300/1500", cfg.SyncSoftDriftMS, cfg.SyncHardDriftMS)
	}
}

func TestLoadFrom_Title_FileWinsOverDefault(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.Title != "File Title" {
		t.Errorf("Title = %q, want file value \"File Title\"", cfg.Title)
	}
}

func TestLoadFrom_Title_EnvWinsOverFile(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SERVER_TITLE", "Env Title")
	fc := fullFileConfig()
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.Title != "Env Title" {
		t.Errorf("Title = %q, want env value \"Env Title\" to win over file", cfg.Title)
	}
}

func TestLoadFrom_BrowserOrigins_FileArray_UsedWhenEnvUnset(t *testing.T) {
	setKeyEnv(t)
	t.Setenv("EMBY_SERVER_URL", "https://emby.example.com")
	fc := fullFileConfig()
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if len(cfg.AppOrigins) != 1 || cfg.AppOrigins[0] != "https://file.example.com" {
		t.Errorf("AppOrigins = %v, want [https://file.example.com] from the file's JSON array", cfg.AppOrigins)
	}
}

func TestLoadFrom_BrowserOrigins_CommaEnvWinsOverFileArray(t *testing.T) {
	setKeyEnv(t)
	t.Setenv("EMBY_SERVER_URL", "https://emby.example.com")
	t.Setenv("APP_ORIGINS", "https://env-a.example.com,https://env-b.example.com")
	fc := fullFileConfig()
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if len(cfg.AppOrigins) != 2 {
		t.Fatalf("AppOrigins = %v, want 2 comma-split entries from env", cfg.AppOrigins)
	}
}

func TestLoadFrom_SessionIdleTimeout_FileDuration_UsedWhenEnvUnset(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SessionIdleTimeout != time.Hour {
		t.Errorf("SessionIdleTimeout = %v, want file value 1h", cfg.SessionIdleTimeout)
	}
}

func TestLoadFrom_SessionIdleTimeout_EnvWinsOverFile(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SESSION_IDLE_TIMEOUT", "45m")
	fc := fullFileConfig()
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SessionIdleTimeout != 45*time.Minute {
		t.Errorf("SessionIdleTimeout = %v, want env value 45m to win over file", cfg.SessionIdleTimeout)
	}
}

func TestLoadFrom_SyncMaxRateAdjustment_FileFloat_UsedWhenEnvUnset(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SyncMaxRateAdjust != 0.2 {
		t.Errorf("SyncMaxRateAdjust = %v, want file value 0.2", cfg.SyncMaxRateAdjust)
	}
}

func TestLoadFrom_SyncMaxRateAdjustment_EnvWinsOverFile(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SYNC_MAX_RATE_ADJUSTMENT", "0.08")
	fc := fullFileConfig()
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SyncMaxRateAdjust != 0.08 {
		t.Errorf("SyncMaxRateAdjust = %v, want env value 0.08 to win over file", cfg.SyncMaxRateAdjust)
	}
}

// --- sync_soft_drift / sync_hard_drift: the one field pair with
// asymmetric env (bare-int ms) vs. file (explicit-unit duration string)
// formats, per Mark's round-2 correction. ---

func TestLoadFrom_Drift_EnvBareIntUnchanged(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SYNC_SOFT_DRIFT_MS", "111")
	cfg, err := loadFrom(nil)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SyncSoftDriftMS != 111 {
		t.Errorf("SyncSoftDriftMS = %d, want 111 from a bare-integer env value", cfg.SyncSoftDriftMS)
	}
}

func TestLoadFrom_Drift_FileRequiresExplicitUnit(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SyncSoftDriftMS != 250 {
		t.Errorf("SyncSoftDriftMS = %d, want 250 from file value \"250ms\"", cfg.SyncSoftDriftMS)
	}
	if cfg.SyncHardDriftMS != 2500 {
		t.Errorf("SyncHardDriftMS = %d, want 2500 from file value \"2500ms\"", cfg.SyncHardDriftMS)
	}
}

func TestLoadFrom_Drift_FileBareNumber_Rejected(t *testing.T) {
	// The critical footgun-prevention case: a unit-less "300" in the file
	// must NOT be silently interpreted as 300 seconds (the convention
	// every other duration field in this config uses) -- that would turn
	// a sub-second drift threshold into a nonsensical five-minute one.
	setRequiredEnv(t)
	fc := fullFileConfig()
	fc.GlobalPlaybackSettings.SyncSoftDrift = strp("300")
	_, err := loadFrom(fc)
	if err == nil {
		t.Fatal("expected an error for a bare-number (no unit) sync_soft_drift file value")
	}
	if !strings.Contains(err.Error(), "sync_soft_drift") {
		t.Errorf("error %q does not name the field", err.Error())
	}
}

func TestLoadFrom_Drift_EnvWinsOverFileDespiteFormatDifference(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SYNC_HARD_DRIFT_MS", "9000")
	fc := fullFileConfig() // file sets sync_hard_drift to "2500ms"
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SyncHardDriftMS != 9000 {
		t.Errorf("SyncHardDriftMS = %d, want env value 9000 to win over file's 2500ms", cfg.SyncHardDriftMS)
	}
}

func TestLoadFrom_Drift_HardMustExceedSoft_FromFile(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	fc.GlobalPlaybackSettings.SyncSoftDrift = strp("2000ms")
	fc.GlobalPlaybackSettings.SyncHardDrift = strp("1000ms")
	_, err := loadFrom(fc)
	if err == nil {
		t.Fatal("expected an error when file-sourced sync_hard_drift <= sync_soft_drift")
	}
}

func TestLoadFrom_Drift_HardMustExceedSoft_MixedSources(t *testing.T) {
	// Soft drift from the file, hard drift from env -- proves the
	// cross-field check operates on the resolved int values regardless of
	// which source (and format) produced each one.
	setRequiredEnv(t)
	t.Setenv("SYNC_HARD_DRIFT_MS", "100")
	fc := fullFileConfig()
	fc.GlobalPlaybackSettings.SyncSoftDrift = strp("500ms")
	_, err := loadFrom(fc)
	if err == nil {
		t.Fatal("expected an error: hard drift (env, 100ms) <= soft drift (file, 500ms)")
	}
}

// --- sync_snapshot_interval grouping regression: must resolve from
// GlobalPlaybackSettings now, not ServerSettings (Mark's round-2 fix). ---

func TestLoadFrom_SyncSnapshotInterval_ResolvesFromGlobalPlaybackSettings(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SyncSnapshotInterval != 7*time.Second {
		t.Errorf("SyncSnapshotInterval = %v, want 7s from global_playback_settings.sync_snapshot_interval", cfg.SyncSnapshotInterval)
	}
}

// --- validation error-naming: field + value, from either source ---

func TestLoadFrom_MalformedListenAddress_FromEnv_NamesFieldAndValue(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LISTEN_ADDR", "bogus")
	_, err := loadFrom(nil)
	if err == nil {
		t.Fatal("expected an error for a malformed LISTEN_ADDR")
	}
	if !strings.Contains(err.Error(), "listen_address") || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q does not name the field and value", err.Error())
	}
}

func TestLoadFrom_MalformedListenAddress_FromFile_NamesFieldAndValue(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	fc.ServerSettings.ListenAddress = strp("bogus")
	_, err := loadFrom(fc)
	if err == nil {
		t.Fatal("expected an error for a malformed file listen_address")
	}
	if !strings.Contains(err.Error(), "listen_address") || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error %q does not name the field and value", err.Error())
	}
}

func TestLoadFrom_InvalidLogLevel_FromEnv_NamesFieldAndValue(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("LOG_LEVEL", "shout")
	_, err := loadFrom(nil)
	if err == nil {
		t.Fatal("expected an error for an invalid LOG_LEVEL")
	}
	if !strings.Contains(err.Error(), "log_level") || !strings.Contains(err.Error(), "shout") {
		t.Errorf("error %q does not name the field and value", err.Error())
	}
}

func TestLoadFrom_InvalidLogLevel_FromFile_NamesFieldAndValue(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	fc.ServerSettings.LogLevel = strp("shout")
	_, err := loadFrom(fc)
	if err == nil {
		t.Fatal("expected an error for an invalid file log_level")
	}
	if !strings.Contains(err.Error(), "log_level") || !strings.Contains(err.Error(), "shout") {
		t.Errorf("error %q does not name the field and value", err.Error())
	}
}

func TestLoadFrom_MalformedDuration_FromEnv_NamesFieldAndValue(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PARTY_INACTIVITY_TIMEOUT", "not-a-duration")
	_, err := loadFrom(nil)
	if err == nil {
		t.Fatal("expected an error for a malformed PARTY_INACTIVITY_TIMEOUT")
	}
	if !strings.Contains(err.Error(), "not-a-duration") {
		t.Errorf("error %q does not name the offending value", err.Error())
	}
}

func TestLoadFrom_MalformedDuration_FromFile_NamesFieldAndValue(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	fc.GlobalPartySettings.InactivityTimeout = strp("not-a-duration")
	_, err := loadFrom(fc)
	if err == nil {
		t.Fatal("expected an error for a malformed file inactivity_timeout")
	}
	if !strings.Contains(err.Error(), "global_party_settings.inactivity_timeout") || !strings.Contains(err.Error(), "not-a-duration") {
		t.Errorf("error %q does not name the field and value", err.Error())
	}
}

func TestLoadFrom_SyncMaxRateAdjustment_OutOfRange_FromFile(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	fc.GlobalPlaybackSettings.SyncMaxRateAdjustment = f64p(1.5)
	_, err := loadFrom(fc)
	if err == nil {
		t.Fatal("expected an error for a file sync_max_rate_adjustment outside (0,1)")
	}
}

// --- Disableable-duration fields (session idle/max-age, progress/snapshot
// intervals, soft/hard drift): a literal zero must be rejected at load
// time -- see ValidateDisableableDuration's doc comment for why zero is
// dangerous (a crash for the two ticker-driven intervals, inverted
// semantics for the rest) rather than merely a no-op. A negative value
// ("disabled") must load successfully. ---

func TestLoadFrom_DisableableDurationFields_ZeroFromFile_Rejected(t *testing.T) {
	setRequiredEnv(t)
	cases := []struct {
		name  string
		apply func(fc *FileConfig)
	}{
		{"session_idle_timeout", func(fc *FileConfig) { fc.ServerSettings.SessionIdleTimeout = strp("0s") }},
		{"session_age_timeout", func(fc *FileConfig) { fc.ServerSettings.SessionAgeTimeout = strp("0s") }},
		{"progress_interval", func(fc *FileConfig) { fc.GlobalPlaybackSettings.ProgressInterval = strp("0s") }},
		{"sync_snapshot_interval", func(fc *FileConfig) { fc.GlobalPlaybackSettings.SyncSnapshotInterval = strp("0s") }},
		{"sync_soft_drift", func(fc *FileConfig) { fc.GlobalPlaybackSettings.SyncSoftDrift = strp("0ms") }},
		{"sync_hard_drift", func(fc *FileConfig) { fc.GlobalPlaybackSettings.SyncHardDrift = strp("0ms") }},
	}
	for _, tc := range cases {
		fc := fullFileConfig()
		tc.apply(fc)
		_, err := loadFrom(fc)
		if err == nil {
			t.Errorf("%s: expected an error for a literal 0 duration, got nil", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%s: error %q does not name the offending field", tc.name, err.Error())
		}
	}
}

func TestLoadFrom_DisableableDurationFields_NegativeFromFile_OK(t *testing.T) {
	setRequiredEnv(t)
	fc := fullFileConfig()
	fc.ServerSettings.SessionIdleTimeout = strp("-1s")
	fc.ServerSettings.SessionAgeTimeout = strp("-1s")
	fc.GlobalPlaybackSettings.ProgressInterval = strp("-1s")
	fc.GlobalPlaybackSettings.SyncSnapshotInterval = strp("-1s")
	fc.GlobalPlaybackSettings.SyncSoftDrift = strp("-1ms")
	fc.GlobalPlaybackSettings.SyncHardDrift = strp("-1ms")
	cfg, err := loadFrom(fc)
	if err != nil {
		t.Fatalf("loadFrom with negative (disabled) durations = %v, want nil", err)
	}
	if cfg.SessionIdleTimeout != -time.Second || cfg.SessionMaxAge != -time.Second {
		t.Errorf("SessionIdleTimeout/SessionMaxAge = %v/%v, want -1s/-1s", cfg.SessionIdleTimeout, cfg.SessionMaxAge)
	}
	if cfg.EmbyProgressInterval != -time.Second || cfg.SyncSnapshotInterval != -time.Second {
		t.Errorf("EmbyProgressInterval/SyncSnapshotInterval = %v/%v, want -1s/-1s", cfg.EmbyProgressInterval, cfg.SyncSnapshotInterval)
	}
	if cfg.SyncSoftDriftMS != -1 || cfg.SyncHardDriftMS != -1 {
		t.Errorf("SyncSoftDriftMS/SyncHardDriftMS = %d/%d, want -1/-1", cfg.SyncSoftDriftMS, cfg.SyncHardDriftMS)
	}
}

func TestLoadFrom_DisableableDurationFields_ZeroFromEnv_Rejected(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("SESSION_IDLE_TIMEOUT", "0")
	_, err := loadFrom(nil)
	if err == nil {
		t.Fatal("expected an error for SESSION_IDLE_TIMEOUT=0")
	}
	if !strings.Contains(err.Error(), "session_idle_timeout") {
		t.Errorf("error %q does not name the offending field", err.Error())
	}
}

// --- TOKEN_ENCRYPTION_KEY: still required on the normal-mode (loadFrom)
// path -- proves the round-2 change narrowed WHERE the check runs
// (setup-required mode no longer needs it -- see setupmode_test.go)
// without weakening it on the path that still needs it. ---

func TestLoadFrom_StillRequiresTokenEncryptionKey(t *testing.T) {
	t.Setenv("APP_ORIGINS", "https://watchparty.example.com")
	t.Setenv("EMBY_SERVER_URL", "https://emby.example.com")
	// Deliberately not calling setKeyEnv/setRequiredEnv's key line.
	_, err := loadFrom(nil)
	if err == nil {
		t.Fatal("expected loadFrom to still require TOKEN_ENCRYPTION_KEY/_FILE")
	}
}
