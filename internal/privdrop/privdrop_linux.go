//go:build linux

package privdrop

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// apply is the real implementation. It requires CGO_ENABLED=0: per the
// standard library's own documentation, syscall.AllThreadsSyscall "always
// returns ENOTSUP in binaries that use cgo" (cgo-created threads aren't
// visible to it) — which this project already builds with regardless (see
// Dockerfile), since modernc.org/sqlite is a pure-Go driver.
func apply(cfg Config) error {
	if os.Geteuid() != 0 {
		return nil
	}

	for _, p := range cfg.Paths {
		if p == "" {
			continue
		}
		if err := chownRecursive(p, cfg.UID, cfg.GID); err != nil {
			return fmt.Errorf("privdrop: chown %s to %d:%d: %w", p, cfg.UID, cfg.GID, err)
		}
	}

	// Order matters: drop supplementary groups, then the primary group,
	// then the user ID last — dropping the UID first would remove
	// permission to change the GID. AllThreadsSyscall (not a plain
	// syscall.Setuid/Setgid) is required here: Go's runtime spreads a
	// process across multiple OS threads, and a plain setuid(2)/setgid(2)
	// syscall only changes credentials for the calling thread, silently
	// leaving other threads (and hence, eventually, other goroutines)
	// still running as root. AllThreadsSyscall applies the change to every
	// OS thread atomically, which is exactly what a credential drop needs.
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_SETGROUPS, 0, 0, 0); errno != 0 {
		return fmt.Errorf("privdrop: setgroups(0): %w", wrapErrno(errno))
	}
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_SETGID, uintptr(cfg.GID), 0, 0); errno != 0 {
		return fmt.Errorf("privdrop: setgid(%d): %w", cfg.GID, wrapErrno(errno))
	}
	if _, _, errno := syscall.AllThreadsSyscall(syscall.SYS_SETUID, uintptr(cfg.UID), 0, 0); errno != 0 {
		return fmt.Errorf("privdrop: setuid(%d): %w", cfg.UID, wrapErrno(errno))
	}
	return nil
}

// wrapErrno adds an actionable hint for the one failure mode that isn't a
// misconfigured UID/GID: ENOTSUP here specifically means the binary was
// built with cgo enabled (AllThreadsSyscall's documented behavior), not
// that the target UID/GID is somehow invalid. The shipped Dockerfile
// already builds with CGO_ENABLED=0; this only bites someone building the
// binary directly (`go build`/`go install`) without that.
func wrapErrno(errno syscall.Errno) error {
	if errno == syscall.ENOTSUP || errno == syscall.EOPNOTSUPP {
		return fmt.Errorf("%w (this binary was built with cgo enabled; rebuild with CGO_ENABLED=0, as the Dockerfile does — AllThreadsSyscall cannot safely drop privileges across cgo-created threads)", errno)
	}
	return errno
}

func chownRecursive(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		return os.Chown(path, uid, gid)
	})
}
