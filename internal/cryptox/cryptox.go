// Package cryptox provides authenticated encryption for secrets stored at
// rest (specifically, Emby AccessTokens persisted in SQLite). See
// ARCHITECTURE.md for why AES-256-GCM was chosen.
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// TokenCipher encrypts and decrypts short secrets (Emby AccessTokens) using
// AES-256-GCM with a random 96-bit nonce prepended to each ciphertext.
type TokenCipher struct {
	aead cipher.AEAD
}

// NewTokenCipher constructs a TokenCipher from a 32-byte key (as produced by
// config.Load's TOKEN_ENCRYPTION_KEY decoding).
func NewTokenCipher(key []byte) (*TokenCipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("cryptox: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cryptox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cryptox: %w", err)
	}
	return &TokenCipher{aead: aead}, nil
}

// Encrypt returns a base64(nonce || ciphertext || tag) string suitable for
// storing in a SQLite TEXT column.
func (c *TokenCipher) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("cryptox: generating nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. It returns an error (never a partial/garbage
// result) if the ciphertext was tampered with or the key is wrong.
func (c *TokenCipher) Decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("cryptox: invalid encoding: %w", err)
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("cryptox: ciphertext too short")
	}
	nonce, ciphertext := raw[:ns], raw[ns:]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("cryptox: decryption failed: %w", err)
	}
	return string(plaintext), nil
}
