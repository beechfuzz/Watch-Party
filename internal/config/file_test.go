package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileExists_True(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	exists, err := FileExists(path)
	if err != nil {
		t.Fatalf("FileExists: %v", err)
	}
	if !exists {
		t.Error("FileExists = false, want true for an existing file")
	}
}

func TestFileExists_False_NotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.jsonc")
	exists, err := FileExists(path)
	if err != nil {
		t.Fatalf("FileExists: %v", err)
	}
	if exists {
		t.Error("FileExists = true, want false for a nonexistent file")
	}
}

func TestFileExists_Error_NotNotExist(t *testing.T) {
	// A path through a non-directory (e.g. treating a regular file as a
	// directory component) produces an os.Stat error that is NOT
	// "not exist" -- this must be surfaced as a hard error, not silently
	// treated as "setup required", per the approved plan.
	dir := t.TempDir()
	regularFile := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(regularFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(regularFile, "config.jsonc")

	exists, err := FileExists(path)
	if err == nil {
		t.Fatalf("FileExists(%q) = (%v, nil), want a non-nil error (path traverses a regular file)", path, exists)
	}
	if os.IsNotExist(err) {
		t.Errorf("FileExists returned an os.IsNotExist-classified error for a non-'not exist' stat failure: %v", err)
	}
}

func TestLoadFile_FullFile_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	content := `{
  // comment
  "server_settings": {
    "title": "My Watch Party",
    "log_level": "debug",
    "browser_origins": ["https://watchparty.example.com", "http://watchparty.home"],
    "listen_address": "0.0.0.0:9090",
    "session_idle_timeout": "12h",
    "session_age_timeout": "360h"
  },
  "global_party_settings": {
    "host_grace_period": "30s",
    "inactivity_timeout": "24h"
  },
  "global_playback_settings": {
    "progress_interval": "5s",
    "sync_snapshot_interval": "2s",
    "sync_soft_drift": "250ms",
    "sync_hard_drift": "2000ms",
    "sync_max_rate_adjustment": 0.1
  },
  "media_server_settings": {
    "server_url": "https://emby.example.com",
    "public_url": "https://emby-public.example.com"
  }
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	fc, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}

	check := func(name string, got, want any) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if fc.ServerSettings.Title == nil {
		t.Fatal("ServerSettings.Title is nil")
	}
	check("ServerSettings.Title", *fc.ServerSettings.Title, "My Watch Party")
	check("ServerSettings.LogLevel", *fc.ServerSettings.LogLevel, "debug")
	if fc.ServerSettings.BrowserOrigins == nil || len(*fc.ServerSettings.BrowserOrigins) != 2 {
		t.Fatalf("ServerSettings.BrowserOrigins = %v, want 2 entries", fc.ServerSettings.BrowserOrigins)
	}
	check("ServerSettings.ListenAddress", *fc.ServerSettings.ListenAddress, "0.0.0.0:9090")
	check("ServerSettings.SessionIdleTimeout", *fc.ServerSettings.SessionIdleTimeout, "12h")
	check("ServerSettings.SessionAgeTimeout", *fc.ServerSettings.SessionAgeTimeout, "360h")
	check("GlobalPartySettings.HostGracePeriod", *fc.GlobalPartySettings.HostGracePeriod, "30s")
	check("GlobalPartySettings.InactivityTimeout", *fc.GlobalPartySettings.InactivityTimeout, "24h")
	check("GlobalPlaybackSettings.ProgressInterval", *fc.GlobalPlaybackSettings.ProgressInterval, "5s")
	check("GlobalPlaybackSettings.SyncSnapshotInterval", *fc.GlobalPlaybackSettings.SyncSnapshotInterval, "2s")
	check("GlobalPlaybackSettings.SyncSoftDrift", *fc.GlobalPlaybackSettings.SyncSoftDrift, "250ms")
	check("GlobalPlaybackSettings.SyncHardDrift", *fc.GlobalPlaybackSettings.SyncHardDrift, "2000ms")
	check("GlobalPlaybackSettings.SyncMaxRateAdjustment", *fc.GlobalPlaybackSettings.SyncMaxRateAdjustment, 0.1)
	check("MediaServerSettings.ServerURL", *fc.MediaServerSettings.ServerURL, "https://emby.example.com")
	check("MediaServerSettings.PublicURL", *fc.MediaServerSettings.PublicURL, "https://emby-public.example.com")
}

func TestLoadFile_OmittedField_LeavesPointerNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	// Deliberately omits server_settings.title and media_server_settings.public_url.
	content := `{
  "server_settings": {
    "log_level": "info",
    "browser_origins": ["https://watchparty.example.com"],
    "listen_address": ":8080",
    "session_idle_timeout": "24h",
    "session_age_timeout": "720h"
  },
  "global_party_settings": { "host_grace_period": "20s", "inactivity_timeout": "48h" },
  "global_playback_settings": {
    "progress_interval": "10s", "sync_snapshot_interval": "4s",
    "sync_soft_drift": "300ms", "sync_hard_drift": "1500ms",
    "sync_max_rate_adjustment": 0.05
  },
  "media_server_settings": { "server_url": "https://emby.example.com" }
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	fc, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}
	if fc.ServerSettings.Title != nil {
		t.Errorf("ServerSettings.Title = %v, want nil (omitted from file)", *fc.ServerSettings.Title)
	}
	if fc.MediaServerSettings.PublicURL != nil {
		t.Errorf("MediaServerSettings.PublicURL = %v, want nil (omitted from file)", *fc.MediaServerSettings.PublicURL)
	}
}

func TestLoadFile_MalformedJSON_ErrorNamesPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	if err := os.WriteFile(path, []byte(`{"server_settings": {`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadFile(path)
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not mention the file path %q", err.Error(), path)
	}
}

func TestLoadFile_UnknownTopLevelKey_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	if err := os.WriteFile(path, []byte(`{"totally_unknown_key": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFile(path); err == nil {
		t.Error("expected an error for an unknown top-level key, got nil")
	}
}

func TestLoadFile_UnknownNestedKey_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	if err := os.WriteFile(path, []byte(`{"server_settings": {"not_a_real_field": 1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFile(path); err == nil {
		t.Error("expected an error for an unknown nested key, got nil")
	}
}

// TestLoadFile_TokenEncryptionKey_RejectedAsUnknownField is a regression
// guard: FileConfig must never grow a field for the encryption key, and
// this proves a file that tries to set one is rejected the same way any
// other typo/unknown key is, rather than silently accepted or silently
// ignored.
func TestLoadFile_TokenEncryptionKey_RejectedAsUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	content := `{"server_settings": {"token_encryption_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFile(path); err == nil {
		t.Error("expected token_encryption_key in the file to be rejected as an unknown field, got nil error")
	}
}

func TestLoadFile_NonexistentPath_Errors(t *testing.T) {
	_, err := loadFile(filepath.Join(t.TempDir(), "nope.jsonc"))
	if err == nil {
		t.Fatal("expected an error for a nonexistent file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap os.ErrNotExist", err)
	}
}
