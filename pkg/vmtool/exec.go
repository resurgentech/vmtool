package vmtool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testStubsEnv names the directory of test stubs a `go test` binary may run
// (internal/testexec.StubsEnv; the same string, kept here so production code
// does not import a test helper).
const testStubsEnv = "VMTOOL_TEST_STUBS"

// command is the one way this package starts an external tool. Outside
// `go test` it is exec.Command.
//
// Under `go test` (testing.Testing()) it resolves name on PATH and allows it
// only when the resolved file is inside the directory named by
// VMTOOL_TEST_STUBS, which internal/testexec.Stub sets. Anything else (real
// ansible, ssh, sshpass, scp, sudo, or any other name) is refused without
// starting a process. The refusal is both returned and stored in the Cmd's
// Err field, so a builder that only returns the *exec.Cmd still fails at
// Start/Run before anything runs.
func command(name string, args ...string) (*exec.Cmd, error) {
	cmd := exec.Command(name, args...)
	if !testing.Testing() {
		return cmd, nil
	}
	if err := allowedInTest(name); err != nil {
		cmd.Err = err
		return cmd, err
	}
	return cmd, nil
}

func allowedInTest(name string) error {
	refuse := fmt.Errorf("test tried to run real %q; install stubs with testexec.Stub(t, ...) (internal/testexec)", name)
	stubs := os.Getenv(testStubsEnv)
	if stubs == "" {
		return refuse
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return refuse
	}
	if !inDir(p, stubs) {
		return refuse
	}
	return nil
}

// inDir reports whether file p lies inside dir, both made absolute and
// symlink-resolved (p's directory, so a stub's own name is kept).
func inDir(p, dir string) bool {
	resolve := func(s string) (string, bool) {
		a, err := filepath.Abs(s)
		if err != nil {
			return "", false
		}
		if r, err := filepath.EvalSymlinks(a); err == nil {
			a = r
		}
		return a, true
	}
	d, ok1 := resolve(dir)
	f, ok2 := resolve(filepath.Dir(p))
	if !ok1 || !ok2 {
		return false
	}
	rel, err := filepath.Rel(d, f)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isTestBinary reports whether exe is a `go test` binary: the running process
// is one (testing.Testing) and exe is the running executable, or exe's base
// name ends in ".test" as `go test` names them.
func isTestBinary(exe string) bool {
	if strings.HasSuffix(filepath.Base(exe), ".test") {
		return true
	}
	if !testing.Testing() {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	return sameFile(self, exe)
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

func testBinaryTunnelError(exe string) error {
	return fmt.Errorf("refusing to use the test binary %s as the vmtool tunnel; set Tunnel.Exe to a stub (internal/testexec)", exe)
}
