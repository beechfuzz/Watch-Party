//go:build race

package privdrop

// Under `go test -race`, the race detector's runtime pulls in cgo (needed
// for TSAN), so AllThreadsSyscall correctly refuses with ENOTSUP rather
// than performing an incomplete privilege drop — see its doc comment ("...
// always returns ENOTSUP in binaries that use cgo"). This is the real,
// documented, safe behavior, not a bug: the production binary always
// builds with CGO_ENABLED=0 (see Dockerfile) and is never raced, so this
// only affects these tests, which skip accordingly.
const raceEnabled = true
