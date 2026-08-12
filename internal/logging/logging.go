// Package logging provides structured JSON logging built on log/slog.
//
// Hard rule (see project spec "Authentication & Security"): never log Emby
// passwords, AccessTokens, session IDs, cookies, Authorization headers, or
// fully authenticated playback URLs, even at LOG_LEVEL=debug. Callers must
// not pass these as log fields — there is no redaction magic here, this is
// enforced by code review discipline at each call site instead of trying to
// pattern-match secrets out of arbitrary strings after the fact.
package logging

import (
	"context"
	"log/slog"
	"os"
)

// New builds the root JSON logger for the process.
func New(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

type ctxKey int

const loggerCtxKey ctxKey = iota

// WithContext attaches a logger (typically already annotated with a
// request_id field) to a context for retrieval deeper in a call chain.
func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerCtxKey, l)
}

// FromContext retrieves the logger attached by WithContext, falling back to
// slog.Default() if none was attached.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerCtxKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
