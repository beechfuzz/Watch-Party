package config

import (
	"os"
	"strings"
	"testing"
)

func TestResolveListenAddress_Default(t *testing.T) {
	got, err := ResolveListenAddress(nil)
	if err != nil {
		t.Fatalf("ResolveListenAddress(nil): %v", err)
	}
	if got != ":8080" {
		t.Errorf("ResolveListenAddress(nil) = %q, want \":8080\"", got)
	}
}

func TestResolveListenAddress_EnvOverride(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "0.0.0.0:9090")
	got, err := ResolveListenAddress(nil)
	if err != nil {
		t.Fatalf("ResolveListenAddress: %v", err)
	}
	if got != "0.0.0.0:9090" {
		t.Errorf("ResolveListenAddress = %q, want \"0.0.0.0:9090\"", got)
	}
}

func TestResolveListenAddress_FileValue_UsedWhenEnvUnset(t *testing.T) {
	fileVal := ":9999"
	got, err := ResolveListenAddress(&fileVal)
	if err != nil {
		t.Fatalf("ResolveListenAddress: %v", err)
	}
	if got != ":9999" {
		t.Errorf("ResolveListenAddress = %q, want \":9999\"", got)
	}
}

func TestResolveListenAddress_EnvWinsOverFile(t *testing.T) {
	t.Setenv("LISTEN_ADDR", ":1111")
	fileVal := ":9999"
	got, err := ResolveListenAddress(&fileVal)
	if err != nil {
		t.Fatalf("ResolveListenAddress: %v", err)
	}
	if got != ":1111" {
		t.Errorf("ResolveListenAddress = %q, want env value \":1111\" to win over file value", got)
	}
}

func TestResolveListenAddress_Malformed_Rejected(t *testing.T) {
	t.Setenv("LISTEN_ADDR", "not-a-valid-address")
	_, err := ResolveListenAddress(nil)
	if err == nil {
		t.Fatal("expected an error for a malformed LISTEN_ADDR")
	}
	if !strings.Contains(err.Error(), "listen_address") || !strings.Contains(err.Error(), "not-a-valid-address") {
		t.Errorf("error %q does not name the field and offending value", err.Error())
	}
}

func TestResolveListenAddress_MalformedFileValue_Rejected(t *testing.T) {
	fileVal := "definitely not an address"
	_, err := ResolveListenAddress(&fileVal)
	if err == nil {
		t.Fatal("expected an error for a malformed file listen_address")
	}
	if !strings.Contains(err.Error(), "listen_address") {
		t.Errorf("error %q does not name the field", err.Error())
	}
}

func TestResolveLogLevel_Default(t *testing.T) {
	got, err := ResolveLogLevel(nil)
	if err != nil {
		t.Fatalf("ResolveLogLevel(nil): %v", err)
	}
	if got != "info" {
		t.Errorf("ResolveLogLevel(nil) = %q, want \"info\"", got)
	}
}

func TestResolveLogLevel_EnvOverride(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")
	got, err := ResolveLogLevel(nil)
	if err != nil {
		t.Fatalf("ResolveLogLevel: %v", err)
	}
	if got != "debug" {
		t.Errorf("ResolveLogLevel = %q, want \"debug\"", got)
	}
}

func TestResolveLogLevel_FileValue_UsedWhenEnvUnset(t *testing.T) {
	fileVal := "warn"
	got, err := ResolveLogLevel(&fileVal)
	if err != nil {
		t.Fatalf("ResolveLogLevel: %v", err)
	}
	if got != "warn" {
		t.Errorf("ResolveLogLevel = %q, want \"warn\"", got)
	}
}

func TestResolveLogLevel_EnvWinsOverFile(t *testing.T) {
	t.Setenv("LOG_LEVEL", "error")
	fileVal := "warn"
	got, err := ResolveLogLevel(&fileVal)
	if err != nil {
		t.Fatalf("ResolveLogLevel: %v", err)
	}
	if got != "error" {
		t.Errorf("ResolveLogLevel = %q, want env value \"error\" to win over file value", got)
	}
}

func TestResolveLogLevel_InvalidEnumValue_Rejected(t *testing.T) {
	t.Setenv("LOG_LEVEL", "verbose")
	_, err := ResolveLogLevel(nil)
	if err == nil {
		t.Fatal("expected an error for an invalid LOG_LEVEL enum value")
	}
	if !strings.Contains(err.Error(), "log_level") || !strings.Contains(err.Error(), "verbose") {
		t.Errorf("error %q does not name the field and offending value", err.Error())
	}
}

func TestResolveLogLevel_InvalidFileValue_Rejected(t *testing.T) {
	fileVal := "verbose"
	_, err := ResolveLogLevel(&fileVal)
	if err == nil {
		t.Fatal("expected an error for an invalid file log_level value")
	}
	if !strings.Contains(err.Error(), "log_level") || !strings.Contains(err.Error(), "verbose") {
		t.Errorf("error %q does not name the field and offending value", err.Error())
	}
}

// TestSetupModeResolvers_DoNotRequireTokenEncryptionKey is the direct
// regression test for Mark's round-2 requirement: setup-required mode
// (which calls exactly these two functions, with nil file values, since
// it has no config.jsonc to read) must not need TOKEN_ENCRYPTION_KEY or
// TOKEN_ENCRYPTION_KEY_FILE set at all. Neither env var is set anywhere in
// this test -- if either function secretly depended on the key, this
// would fail.
func TestSetupModeResolvers_DoNotRequireTokenEncryptionKey(t *testing.T) {
	if _, ok := os.LookupEnv("TOKEN_ENCRYPTION_KEY"); ok {
		t.Fatal("test environment leaked TOKEN_ENCRYPTION_KEY; this test needs it unset to be meaningful")
	}
	if _, ok := os.LookupEnv("TOKEN_ENCRYPTION_KEY_FILE"); ok {
		t.Fatal("test environment leaked TOKEN_ENCRYPTION_KEY_FILE; this test needs it unset to be meaningful")
	}

	if _, err := ResolveListenAddress(nil); err != nil {
		t.Errorf("ResolveListenAddress(nil) failed with no encryption key set: %v", err)
	}
	if _, err := ResolveLogLevel(nil); err != nil {
		t.Errorf("ResolveLogLevel(nil) failed with no encryption key set: %v", err)
	}
}
