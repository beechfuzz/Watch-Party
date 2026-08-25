package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKeyB64 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" // 32 zero bytes, base64

func TestResolveTokenEncryptionKey_RawEnvOnly(t *testing.T) {
	t.Setenv("TOKEN_ENCRYPTION_KEY", testKeyB64)
	key, err := resolveTokenEncryptionKey()
	if err != nil {
		t.Fatalf("resolveTokenEncryptionKey: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("key length = %d, want 32", len(key))
	}
}

func TestResolveTokenEncryptionKey_FileOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte(testKeyB64+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN_ENCRYPTION_KEY_FILE", path)
	key, err := resolveTokenEncryptionKey()
	if err != nil {
		t.Fatalf("resolveTokenEncryptionKey: %v", err)
	}
	if len(key) != 32 {
		t.Errorf("key length = %d, want 32", len(key))
	}
}

func TestResolveTokenEncryptionKey_BothSet_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte(testKeyB64), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN_ENCRYPTION_KEY", testKeyB64)
	t.Setenv("TOKEN_ENCRYPTION_KEY_FILE", path)

	_, err := resolveTokenEncryptionKey()
	if err == nil {
		t.Fatal("expected an error when both TOKEN_ENCRYPTION_KEY and TOKEN_ENCRYPTION_KEY_FILE are set")
	}
	if !strings.Contains(err.Error(), "TOKEN_ENCRYPTION_KEY") || !strings.Contains(err.Error(), "TOKEN_ENCRYPTION_KEY_FILE") {
		t.Errorf("error %q does not name both env vars", err.Error())
	}
}

func TestResolveTokenEncryptionKey_NeitherSet_Rejected(t *testing.T) {
	_, err := resolveTokenEncryptionKey()
	if err == nil {
		t.Fatal("expected an error when neither TOKEN_ENCRYPTION_KEY nor TOKEN_ENCRYPTION_KEY_FILE is set")
	}
	if !strings.Contains(err.Error(), "TOKEN_ENCRYPTION_KEY") || !strings.Contains(err.Error(), "TOKEN_ENCRYPTION_KEY_FILE") {
		t.Errorf("error %q does not name both env vars", err.Error())
	}
}

func TestResolveTokenEncryptionKey_FileUnreadable_ErrorWrapsPathNotContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist")
	t.Setenv("TOKEN_ENCRYPTION_KEY_FILE", path)

	_, err := resolveTokenEncryptionKey()
	if err == nil {
		t.Fatal("expected an error for a nonexistent TOKEN_ENCRYPTION_KEY_FILE path")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not mention the file path %q", err.Error(), path)
	}
}

func TestResolveTokenEncryptionKey_FileContentsWrongLength_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("not-a-valid-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOKEN_ENCRYPTION_KEY_FILE", path)

	_, err := resolveTokenEncryptionKey()
	if err == nil {
		t.Fatal("expected an error for a key file whose contents don't decode to 32 bytes")
	}
	if strings.Contains(err.Error(), "not-a-valid-key") {
		t.Errorf("error leaked the raw file contents: %q", err.Error())
	}
}
