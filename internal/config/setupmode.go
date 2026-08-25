package config

import (
	"fmt"
	"strings"
)

// ResolveListenAddress and ResolveLogLevel are exported because they are
// shared by two callers: the full env+file config merge in Load/loadFrom,
// and main.go's setup-required startup path, which has no config.jsonc to
// read (that's precisely why it's in that branch) but still needs a
// listen address and log level to serve the "setup required" placeholder.
//
// Neither function touches TOKEN_ENCRYPTION_KEY / TOKEN_ENCRYPTION_KEY_FILE
// (see tokenkey.go) or requires config.jsonc to exist -- setup-required
// mode must be reachable without either env var set, since the wizard (a
// later phase) is part of how an operator finds out about that
// requirement in the first place.

// ResolveListenAddress resolves LISTEN_ADDR (if set) or fileVal (if not
// nil/empty) or the ":8080" default, and validates that the result parses
// as a usable host:port (or bare ":port") address.
func ResolveListenAddress(fileVal *string) (string, error) {
	addr := resolveString("LISTEN_ADDR", fileVal, ":8080")
	if err := validateListenAddress(addr); err != nil {
		return "", fmt.Errorf("listen_address: %w", err)
	}
	return addr, nil
}

// ResolveLogLevel resolves LOG_LEVEL (if set) or fileVal (if not
// nil/empty) or the "info" default, and validates it's a recognized level.
func ResolveLogLevel(fileVal *string) (string, error) {
	level := strings.ToLower(resolveString("LOG_LEVEL", fileVal, "info"))
	if err := validateLogLevel(level); err != nil {
		return "", fmt.Errorf("log_level: %w", err)
	}
	return level, nil
}
