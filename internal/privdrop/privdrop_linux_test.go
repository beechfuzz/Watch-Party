//go:build linux

package privdrop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// These tests exercise the real, irreversible privilege-drop syscalls, so
// each does its actual work in a re-exec'd subprocess (the standard Go
// pattern for testing things a test process can't safely undo on itself)
// rather than in the test binary's own process.

func TestApply_DropsPrivilegesAndChownsPaths(t *testing.T) {
	if os.Getenv("PRIVDROP_TEST_HELPER") == "1" {
		runHelperApply()
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise the real privilege-drop path")
	}
	if raceEnabled {
		t.Skip("AllThreadsSyscall returns ENOTSUP under -race (its runtime pulls in cgo); see race_on_test.go")
	}

	dir := t.TempDir()
	preexisting := filepath.Join(dir, "preexisting.txt")
	if err := os.WriteFile(preexisting, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	const targetUID, targetGID = 65532, 65532

	cmd := exec.Command(os.Args[0], "-test.run=^TestApply_DropsPrivilegesAndChownsPaths$")
	cmd.Env = append(os.Environ(),
		"PRIVDROP_TEST_HELPER=1",
		"PRIVDROP_TEST_DIR="+dir,
		fmt.Sprintf("PRIVDROP_TEST_UID=%d", targetUID),
		fmt.Sprintf("PRIVDROP_TEST_GID=%d", targetGID),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess failed: %v\noutput: %s", err, out)
	}

	var gotUID, gotGID int
	if _, err := fmt.Sscanf(string(out), "%d %d", &gotUID, &gotGID); err != nil {
		t.Fatalf("could not parse subprocess output %q: %v", out, err)
	}
	if gotUID != targetUID || gotGID != targetGID {
		t.Errorf("subprocess reported uid=%d gid=%d, want uid=%d gid=%d", gotUID, gotGID, targetUID, targetGID)
	}

	// The pre-existing (root-owned) file must have been chowned to the
	// target uid/gid before privileges were dropped.
	info, err := os.Stat(preexisting)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if int(st.Uid) != targetUID || int(st.Gid) != targetGID {
		t.Errorf("preexisting file owner = %d:%d, want %d:%d", st.Uid, st.Gid, targetUID, targetGID)
	}
}

// runHelperApply is the subprocess side of TestApply_DropsPrivilegesAndChownsPaths.
func runHelperApply() {
	dir := os.Getenv("PRIVDROP_TEST_DIR")
	var uid, gid int
	fmt.Sscanf(os.Getenv("PRIVDROP_TEST_UID"), "%d", &uid)
	fmt.Sscanf(os.Getenv("PRIVDROP_TEST_GID"), "%d", &gid)

	if err := Apply(Config{UID: uid, GID: gid, Paths: []string{dir}}); err != nil {
		fmt.Fprintln(os.Stderr, "apply error:", err)
		os.Exit(1)
	}
	fmt.Printf("%d %d\n", os.Getuid(), os.Getgid())
	os.Exit(0)
}

// TestApply_NotRoot_IsNoOp proves Apply no-ops once the process is no
// longer root — a second call, after a first genuinely-root call has
// already dropped privileges, must leave the process exactly where the
// first call put it, ignoring the second call's target entirely.
//
// This deliberately avoids exec.Cmd's SysProcAttr.Credential (starting a
// subprocess pre-dropped to a given uid/gid via the low-level fork/exec
// credential path) — that mechanism is blocked by this sandbox's seccomp
// profile even under euid 0, which is an environment restriction, not a
// bug in privdrop; TestApply_DropsPrivilegesAndChownsPaths above already
// proves the real in-process AllThreadsSyscall path works, so this test
// reuses that same proven path to get itself to a known non-root state
// first.
func TestApply_NotRoot_IsNoOp(t *testing.T) {
	if os.Getenv("PRIVDROP_TEST_HELPER2") == "1" {
		runHelperNotRoot()
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root to exercise the real privilege-drop path")
	}
	if raceEnabled {
		t.Skip("AllThreadsSyscall returns ENOTSUP under -race (its runtime pulls in cgo); see race_on_test.go")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestApply_NotRoot_IsNoOp$")
	cmd.Env = append(os.Environ(), "PRIVDROP_TEST_HELPER2=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess failed: %v\noutput: %s", err, out)
	}

	var gotUID, gotGID int
	if _, err := fmt.Sscanf(string(out), "%d %d", &gotUID, &gotGID); err != nil {
		t.Fatalf("could not parse subprocess output %q: %v", out, err)
	}
	// Must equal the first (genuine) drop target, not the second call's.
	if gotUID != 65534 || gotGID != 65534 {
		t.Errorf("subprocess uid/gid = %d:%d, want 65534:65534 (second Apply call should have no-op'd once no longer root)", gotUID, gotGID)
	}
}

// runHelperNotRoot is the subprocess side of TestApply_NotRoot_IsNoOp.
func runHelperNotRoot() {
	// First call: still root, genuinely drops privileges.
	if err := Apply(Config{UID: 65534, GID: 65534}); err != nil {
		fmt.Fprintln(os.Stderr, "first apply failed:", err)
		os.Exit(1)
	}
	// Second call: no longer root, must be a complete no-op regardless of
	// the (different) target given.
	if err := Apply(Config{UID: 1, GID: 1}); err != nil {
		fmt.Fprintln(os.Stderr, "second apply failed (should have been a no-op):", err)
		os.Exit(1)
	}
	fmt.Printf("%d %d\n", os.Getuid(), os.Getgid())
	os.Exit(0)
}
