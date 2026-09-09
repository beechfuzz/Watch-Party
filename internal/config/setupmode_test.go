package config

import (
	"os"
	"strings"
	"testing"
	"time"
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

// --- Direct validator tests. These functions were exported specifically
// so the setup wizard can validate an operator's raw form input without
// going through ResolveListenAddress/ResolveLogLevel (which would consult
// the environment instead of validating what was actually submitted -- see
// ARCHITECTURE.md §16 round-2 investigation notes) or reimplementing the
// loader's cross-field/duration-parsing rules a second time. ---

func TestValidateListenAddress_Valid(t *testing.T) {
	for _, addr := range []string{":8080", "0.0.0.0:8080", "127.0.0.1:9090", ":0"} {
		if err := ValidateListenAddress(addr); err != nil {
			t.Errorf("ValidateListenAddress(%q) = %v, want nil", addr, err)
		}
	}
}

func TestValidateListenAddress_Invalid(t *testing.T) {
	for _, addr := range []string{"", "not-an-address", "localhost", ":notaport", ":99999"} {
		if err := ValidateListenAddress(addr); err == nil {
			t.Errorf("ValidateListenAddress(%q) = nil, want an error", addr)
		}
	}
}

func TestValidateLogLevel_Valid(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		if err := ValidateLogLevel(level); err != nil {
			t.Errorf("ValidateLogLevel(%q) = %v, want nil", level, err)
		}
	}
}

func TestValidateLogLevel_Invalid(t *testing.T) {
	for _, level := range []string{"", "verbose", "INFO", "trace"} {
		if err := ValidateLogLevel(level); err == nil {
			t.Errorf("ValidateLogLevel(%q) = nil, want an error", level)
		}
	}
}

func TestValidateTitle_Valid(t *testing.T) {
	if err := ValidateTitle("Watch Party"); err != nil {
		t.Errorf("ValidateTitle(\"Watch Party\") = %v, want nil", err)
	}
}

func TestValidateTitle_BlankOrWhitespace_Rejected(t *testing.T) {
	for _, title := range []string{"", "   ", "\t\n"} {
		if err := ValidateTitle(title); err == nil {
			t.Errorf("ValidateTitle(%q) = nil, want an error", title)
		}
	}
}

func TestValidateOrigins_Valid(t *testing.T) {
	got, err := ValidateOrigins([]string{" https://a.example.com/ ", "http://b.example.com"})
	if err != nil {
		t.Fatalf("ValidateOrigins: %v", err)
	}
	want := []string{"https://a.example.com", "http://b.example.com"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ValidateOrigins = %v, want %v (trimmed, trailing slash stripped)", got, want)
	}
}

func TestValidateOrigins_EmptyAfterFiltering_Rejected(t *testing.T) {
	for _, raw := range [][]string{{}, {""}, {"  ", "\t"}} {
		if _, err := ValidateOrigins(raw); err == nil {
			t.Errorf("ValidateOrigins(%v) = nil, want an error", raw)
		}
	}
}

func TestValidateSyncDrift_HardGreaterThanSoft_OK(t *testing.T) {
	if err := ValidateSyncDrift(300*time.Millisecond, 1500*time.Millisecond); err != nil {
		t.Errorf("ValidateSyncDrift(300ms, 1500ms) = %v, want nil", err)
	}
}

func TestValidateSyncDrift_HardNotGreaterThanSoft_Rejected(t *testing.T) {
	cases := []struct{ soft, hard time.Duration }{
		{300 * time.Millisecond, 300 * time.Millisecond},  // equal
		{1500 * time.Millisecond, 300 * time.Millisecond}, // hard < soft
	}
	for _, tc := range cases {
		if err := ValidateSyncDrift(tc.soft, tc.hard); err == nil {
			t.Errorf("ValidateSyncDrift(%v, %v) = nil, want an error", tc.soft, tc.hard)
		}
	}
}

// TestValidateSyncDrift_DisabledSide_ExemptsOrderingCheck: a negative value
// means "disabled" (see ValidateDisableableDuration); ordering between
// "disabled" and any other value -- including another disabled value --
// isn't meaningful, so the hard>soft check must not fire when either side
// is negative, even in the otherwise-rejected equal/hard<soft shapes.
func TestValidateSyncDrift_DisabledSide_ExemptsOrderingCheck(t *testing.T) {
	cases := []struct{ soft, hard time.Duration }{
		{-1 * time.Second, 1500 * time.Millisecond}, // soft disabled, hard real
		{300 * time.Millisecond, -1 * time.Second},  // hard disabled, soft real
		{-1 * time.Second, -1 * time.Second},        // both disabled, equal -- would fail hard<=soft if not exempted
		{-1 * time.Second, -2 * time.Second},        // both disabled, "hard" more negative than "soft"
	}
	for _, tc := range cases {
		if err := ValidateSyncDrift(tc.soft, tc.hard); err != nil {
			t.Errorf("ValidateSyncDrift(%v, %v) = %v, want nil (disabled side exempts ordering)", tc.soft, tc.hard, err)
		}
	}
}

func TestValidateDisableableDuration_Zero_Rejected(t *testing.T) {
	if err := ValidateDisableableDuration("session_idle_timeout", 0); err == nil {
		t.Error("ValidateDisableableDuration(field, 0) = nil, want an error")
	}
}

func TestValidateDisableableDuration_PositiveOrNegative_OK(t *testing.T) {
	for _, d := range []time.Duration{1, time.Second, 24 * time.Hour, -1, -time.Second, -24 * time.Hour} {
		if err := ValidateDisableableDuration("session_idle_timeout", d); err != nil {
			t.Errorf("ValidateDisableableDuration(field, %v) = %v, want nil", d, err)
		}
	}
}

func TestValidateMaxRateAdjustment_InRange_OK(t *testing.T) {
	for _, v := range []float64{0.01, 0.05, 0.5, 0.99} {
		if err := ValidateMaxRateAdjustment(v); err != nil {
			t.Errorf("ValidateMaxRateAdjustment(%v) = %v, want nil", v, err)
		}
	}
}

func TestValidateMaxRateAdjustment_OutOfRange_Rejected(t *testing.T) {
	for _, v := range []float64{0, -0.1, 1, 1.5} {
		if err := ValidateMaxRateAdjustment(v); err == nil {
			t.Errorf("ValidateMaxRateAdjustment(%v) = nil, want an error", v)
		}
	}
}

func TestParseDuration_BareIntAndUnitString(t *testing.T) {
	d, err := ParseDuration("30")
	if err != nil || d != 30*time.Second {
		t.Errorf("ParseDuration(\"30\") = %v, %v, want 30s, nil", d, err)
	}
	d, err = ParseDuration("24h")
	if err != nil || d != 24*time.Hour {
		t.Errorf("ParseDuration(\"24h\") = %v, %v, want 24h, nil", d, err)
	}
}

func TestParseDuration_Invalid_Rejected(t *testing.T) {
	if _, err := ParseDuration("not-a-duration"); err == nil {
		t.Error("ParseDuration(\"not-a-duration\") = nil, want an error")
	}
}

func TestParseDurationStrict_RequiresExplicitUnit(t *testing.T) {
	if _, err := ParseDurationStrict("300"); err == nil {
		t.Error("ParseDurationStrict(\"300\") = nil, want an error (no bare-integer fallback)")
	}
	d, err := ParseDurationStrict("300ms")
	if err != nil || d != 300*time.Millisecond {
		t.Errorf("ParseDurationStrict(\"300ms\") = %v, %v, want 300ms, nil", d, err)
	}
}
