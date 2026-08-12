// Package idgen generates cryptographically random identifiers: opaque
// session IDs and public party IDs. Both must be unguessable — session IDs
// because they are bearer credentials, party IDs because the spec requires
// they not be enumerable (unlike sequential database primary keys).
package idgen

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"fmt"
)

// SessionID returns a 256-bit random opaque token, hex-encoded (64 chars).
func SessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("idgen: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// base32Encoding is unpadded, lowercase Crockford-ish alphabet substitute:
// stdlib doesn't ship Crockford, so we use standard base32 lowercased and
// stripped of padding. It's URL-safe and avoids the '+', '/' of base64 so
// party IDs look clean in a browser address bar.
var base32Encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// PartyID returns a 128-bit random public identifier suitable for use in
// URLs, lowercase base32 (26 chars, no padding, no ambiguous characters
// beyond what base32 already avoids).
func PartyID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("idgen: %w", err)
	}
	return toLower(base32Encoding.EncodeToString(b)), nil
}

func toLower(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
