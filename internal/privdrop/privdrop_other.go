//go:build !linux

package privdrop

// apply is a no-op outside Linux: the PUID/PGID feature only matters for
// the shipped container image, which is always Linux. This keeps `go
// build`/`go test` working for contributors on macOS/Windows without
// pulling in any Linux-only syscalls, rather than making PUID/PGID
// silently fail in a way that would need documenting per-platform.
func apply(cfg Config) error {
	return nil
}
