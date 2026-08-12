package cryptox

import (
	"crypto/rand"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	c, err := NewTokenCipher(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := "super-secret-emby-access-token"
	ct, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, plaintext) {
		t.Error("ciphertext contains plaintext")
	}
	got, err := c.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if got != plaintext {
		t.Errorf("got %q, want %q", got, plaintext)
	}
}

func TestEncryptIsNonDeterministic(t *testing.T) {
	c, _ := NewTokenCipher(testKey(t))
	a, _ := c.Encrypt("token")
	b, _ := c.Encrypt("token")
	if a == b {
		t.Error("two encryptions of the same plaintext produced identical ciphertext (nonce reuse?)")
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	c, _ := NewTokenCipher(testKey(t))
	ct, _ := c.Encrypt("token")
	tampered := []byte(ct)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := c.Decrypt(string(tampered)); err == nil {
		t.Error("expected error decrypting tampered ciphertext")
	}
}

func TestDecryptWrongKeyFails(t *testing.T) {
	c1, _ := NewTokenCipher(testKey(t))
	c2, _ := NewTokenCipher(testKey(t))
	ct, _ := c1.Encrypt("token")
	if _, err := c2.Decrypt(ct); err == nil {
		t.Error("expected error decrypting with wrong key")
	}
}

func TestNewTokenCipher_RejectsWrongKeySize(t *testing.T) {
	if _, err := NewTokenCipher([]byte("too-short")); err == nil {
		t.Error("expected error for non-32-byte key")
	}
}
