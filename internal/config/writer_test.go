package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validFileConfig returns a fully-populated FileConfig matching the
// defaults documented in README.md / setup wizard §5, suitable as a
// baseline for writer tests to mutate one field at a time.
func validFileConfig() *FileConfig {
	fc := &FileConfig{}
	fc.ServerSettings.Title = strp("Watch Party")
	fc.ServerSettings.LogLevel = strp("info")
	fc.ServerSettings.BrowserOrigins = &[]string{"https://watchparty.example.com"}
	fc.ServerSettings.ListenAddress = strp(":8080")
	fc.ServerSettings.SessionIdleTimeout = strp("24h")
	fc.ServerSettings.SessionAgeTimeout = strp("720h")
	fc.GlobalPartySettings.HostGracePeriod = strp("20s")
	fc.GlobalPartySettings.InactivityTimeout = strp("48h")
	fc.GlobalPlaybackSettings.ProgressInterval = strp("10s")
	fc.GlobalPlaybackSettings.SyncSnapshotInterval = strp("4s")
	fc.GlobalPlaybackSettings.SyncSoftDrift = strp("300ms")
	fc.GlobalPlaybackSettings.SyncHardDrift = strp("1500ms")
	rate := 0.05
	fc.GlobalPlaybackSettings.SyncMaxRateAdjustment = &rate
	fc.MediaServerSettings.ServerURL = strp("https://emby.example.com")
	fc.MediaServerSettings.PublicURL = strp("")
	return fc
}

func TestWriteFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	fc := validFileConfig()

	if err := WriteFile(path, fc); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile of written file: %v", err)
	}
	if *got.ServerSettings.Title != "Watch Party" {
		t.Errorf("Title = %q, want %q", *got.ServerSettings.Title, "Watch Party")
	}
	if *got.ServerSettings.ListenAddress != ":8080" {
		t.Errorf("ListenAddress = %q, want %q", *got.ServerSettings.ListenAddress, ":8080")
	}
	if len(*got.ServerSettings.BrowserOrigins) != 1 || (*got.ServerSettings.BrowserOrigins)[0] != "https://watchparty.example.com" {
		t.Errorf("BrowserOrigins = %v, want [https://watchparty.example.com]", *got.ServerSettings.BrowserOrigins)
	}
	if *got.GlobalPlaybackSettings.SyncSoftDrift != "300ms" {
		t.Errorf("SyncSoftDrift = %q, want %q", *got.GlobalPlaybackSettings.SyncSoftDrift, "300ms")
	}
	if *got.GlobalPlaybackSettings.SyncMaxRateAdjustment != 0.05 {
		t.Errorf("SyncMaxRateAdjustment = %v, want 0.05", *got.GlobalPlaybackSettings.SyncMaxRateAdjustment)
	}
	if *got.MediaServerSettings.ServerURL != "https://emby.example.com" {
		t.Errorf("ServerURL = %q, want %q", *got.MediaServerSettings.ServerURL, "https://emby.example.com")
	}

	// Round-trip all the way through loadFrom too, proving the written
	// file is not just parseable but actually loads into a full Config.
	// loadFrom also resolves TOKEN_ENCRYPTION_KEY (env-only, never part of
	// FileConfig -- see WriteFile's doc comment), so it must be set here
	// purely to make loadFrom itself succeed; it plays no role in what
	// this test is actually checking.
	t.Setenv("TOKEN_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	cfg, err := loadFrom(got)
	if err != nil {
		t.Fatalf("loadFrom of round-tripped file: %v", err)
	}
	if cfg.Title != "Watch Party" {
		t.Errorf("Config.Title = %q, want %q", cfg.Title, "Watch Party")
	}
}

func TestWriteFile_AtomicityDeliberateAndNoInvalidFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	fc := validFileConfig()
	fc.ServerSettings.Title = strp("   ") // fails ValidateTitle

	if err := WriteFile(path, fc); err == nil {
		t.Fatal("expected WriteFile to fail for a blank title")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no file at %q after a failed WriteFile, stat err = %v", path, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("stray file left behind after failed WriteFile: %s", e.Name())
	}
}

func TestWriteFile_SecondCallOverwritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	fc := validFileConfig()
	if err := WriteFile(path, fc); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}

	fc2 := validFileConfig()
	fc2.ServerSettings.Title = strp("Renamed Party")
	if err := WriteFile(path, fc2); err != nil {
		t.Fatalf("second WriteFile: %v", err)
	}

	got, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}
	if *got.ServerSettings.Title != "Renamed Party" {
		t.Errorf("Title after second write = %q, want %q", *got.ServerSettings.Title, "Renamed Party")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries after two writes, want exactly 1 (no leftover temp files)", len(entries))
	}
}

func TestWriteFile_MkdirAllCreatesConfigDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config", "config.jsonc")

	if err := WriteFile(path, validFileConfig()); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file at %q, stat err = %v", path, err)
	}
}

// TestWriteFile_DriftFields_AlwaysExplicitUnit proves the written
// sync_soft_drift/sync_hard_drift values always carry an explicit unit
// suffix, matching ParseDurationStrict's requirement -- guaranteed here by
// construction (renderData rejects anything ParseDurationStrict rejects
// before a single byte is written), verified directly against the raw
// written bytes rather than trusting the mechanism blindly.
func TestWriteFile_DriftFields_AlwaysExplicitUnit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	if err := WriteFile(path, validFileConfig()); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, `"sync_soft_drift": "300ms"`) {
		t.Errorf("written file missing unit-suffixed sync_soft_drift; got:\n%s", content)
	}
	if !strings.Contains(content, `"sync_hard_drift": "1500ms"`) {
		t.Errorf("written file missing unit-suffixed sync_hard_drift; got:\n%s", content)
	}
}

func TestWriteFile_DriftFieldWithoutUnit_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	fc := validFileConfig()
	fc.GlobalPlaybackSettings.SyncSoftDrift = strp("300") // no unit

	if err := WriteFile(path, fc); err == nil {
		t.Fatal("expected WriteFile to reject a bare-number sync_soft_drift")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no file left behind, stat err = %v", err)
	}
}

func TestWriteFile_HardDriftNotGreaterThanSoft_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	fc := validFileConfig()
	fc.GlobalPlaybackSettings.SyncSoftDrift = strp("2000ms")
	fc.GlobalPlaybackSettings.SyncHardDrift = strp("1000ms")

	if err := WriteFile(path, fc); err == nil {
		t.Fatal("expected WriteFile to reject sync_hard_drift <= sync_soft_drift")
	}
}

func TestWriteFile_MissingRequiredField_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	fc := validFileConfig()
	fc.MediaServerSettings.ServerURL = nil

	if err := WriteFile(path, fc); err == nil {
		t.Fatal("expected WriteFile to reject a nil required field")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no file left behind, stat err = %v", err)
	}
}

func TestWriteFile_PublicURLOptional(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	fc := validFileConfig()
	fc.MediaServerSettings.PublicURL = nil // omitted, not just blank

	if err := WriteFile(path, fc); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}
	if got.MediaServerSettings.PublicURL == nil || *got.MediaServerSettings.PublicURL != "" {
		t.Errorf("PublicURL = %v, want empty string", got.MediaServerSettings.PublicURL)
	}
}

// TestWriteFile_CommentsPreserved is a smoke check that the written file
// actually contains explanatory `//` comments (per README's documented
// style), not just a bare JSON marshal -- and that they don't break
// parsing (stripJSONC + loadFile already prove that above, but this
// checks the comment text itself is present in the raw bytes).
func TestWriteFile_CommentsPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	if err := WriteFile(path, validFileConfig()); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "//") {
		t.Error("written config.jsonc has no // comments")
	}
	if !strings.Contains(content, "TOKEN_ENCRYPTION_KEY") {
		t.Error("written config.jsonc doesn't mention TOKEN_ENCRYPTION_KEY (expected in the header comment)")
	}
}

func TestWriteFile_SpecialCharactersEscapedSafely(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	fc := validFileConfig()
	fc.ServerSettings.Title = strp(`My "Party" \ Server`)

	if err := WriteFile(path, fc); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := loadFile(path)
	if err != nil {
		t.Fatalf("loadFile: %v", err)
	}
	if *got.ServerSettings.Title != `My "Party" \ Server` {
		t.Errorf("Title = %q, want the original round-tripped exactly", *got.ServerSettings.Title)
	}
}

func TestWriteFile_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	if err := WriteFile(path, validFileConfig()); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("file permissions = %o, want 0644", perm)
	}
}

func TestWriteFile_NilFileConfig_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")

	if err := WriteFile(path, nil); err == nil {
		t.Fatal("expected WriteFile(path, nil) to fail")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected no file left behind, stat err = %v", err)
	}
}
