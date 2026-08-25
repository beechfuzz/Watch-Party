package config

import (
	"errors"
	"fmt"
	"os"
)

// ErrTokenEncryptionKeyRequired wraps the error returned when neither
// TOKEN_ENCRYPTION_KEY nor TOKEN_ENCRYPTION_KEY_FILE is set, so a caller
// can distinguish this specific, expected condition from any other config
// failure via errors.Is. cmd/server's run loop uses this: right after the
// setup wizard writes a config.jsonc it has already validated field-by-
// field, the only way LoadFromPath can still fail is either this (the
// wizard deliberately never writes the key -- see FileConfig's doc
// comment) or a genuinely unexpected problem, and the two need different
// handling (wait for a restart vs. a hard error).
var ErrTokenEncryptionKeyRequired = errors.New("one of TOKEN_ENCRYPTION_KEY or TOKEN_ENCRYPTION_KEY_FILE is required (32-byte key, base64 or hex encoded; generate with: openssl rand -base64 32)")

// resolveTokenEncryptionKey resolves the AES-256-GCM key used to encrypt
// Emby AccessTokens at rest, from exactly one of two environment
// variables:
//
//   - TOKEN_ENCRYPTION_KEY: the raw key value (base64 or hex, 32 bytes).
//   - TOKEN_ENCRYPTION_KEY_FILE: a path to a file containing that value --
//     e.g. a Docker/Podman secret mounted at /run/secrets/....
//
// If both are set, or neither is set, the caller refuses to start rather
// than silently preferring one or falling back to an insecure default.
//
// The key never lives in config.jsonc, in any form -- see FileConfig's
// doc comment. This function is called only from the normal-mode config
// load path (Load/loadFrom): setup-required mode does not open the
// database, construct the token cipher, or do anything else that needs
// this key, so it is never required just to see the "setup required"
// placeholder.
func resolveTokenEncryptionKey() ([]byte, error) {
	raw, rawSet := os.LookupEnv("TOKEN_ENCRYPTION_KEY")
	path, pathSet := os.LookupEnv("TOKEN_ENCRYPTION_KEY_FILE")

	switch {
	case rawSet && pathSet:
		return nil, fmt.Errorf("exactly one of TOKEN_ENCRYPTION_KEY or TOKEN_ENCRYPTION_KEY_FILE must be set, but both are set")
	case !rawSet && !pathSet:
		return nil, ErrTokenEncryptionKeyRequired
	case rawSet:
		key, err := decodeKey(raw)
		if err != nil {
			return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY: %w", err)
		}
		return key, nil
	default:
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY_FILE: reading %q: %w", path, err)
		}
		key, err := decodeKey(string(b))
		if err != nil {
			return nil, fmt.Errorf("TOKEN_ENCRYPTION_KEY_FILE (%s): %w", path, err)
		}
		return key, nil
	}
}
