// Package privdrop lets the server start as root (needed only so it can
// take ownership of a freshly-mounted, possibly root-owned data volume)
// and then permanently drop to an operator-specified unprivileged UID/GID
// before doing anything else — the common PUID/PGID pattern self-hosted
// container images use. See ARCHITECTURE.md and README.md.
//
// This has no effect, and is not required, outside that container image:
// if the process isn't running as root to begin with (the normal case for
// `go run` during local development), Apply is a no-op.
package privdrop

// Config specifies the target UID/GID to drop to, and the paths that need
// to be recursively chowned to them before privileges are dropped — since
// chowning arbitrary paths requires root, that must happen first, and
// arbitrary paths mainly matters for data left behind by a previous run
// under a different UID (e.g. an operator changing PUID after upgrading
// from an image version that predates this feature).
type Config struct {
	UID   int
	GID   int
	Paths []string
}

// Apply chowns Config.Paths and drops privileges as described above. It is
// a no-op if the current process is not running as root (euid != 0).
//
// Implemented per-platform: see privdrop_linux.go for the real
// implementation and privdrop_other.go for the no-op used everywhere else.
func Apply(cfg Config) error {
	return apply(cfg)
}
