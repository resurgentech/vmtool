package vmtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/testexec"
)

// stubAnsible installs an `ansible` stub (internal/testexec) that appends
// its argv to a log and prints out, exiting with code. It returns the log path.
func stubAnsible(t *testing.T, out string, code int) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "argv.log")
	script := "#!/bin/sh\necho \"$@\" >> '" + log + "'\ncat <<'EOF'\n" + out + "\nEOF\nexit " +
		string(rune('0'+code)) + "\n"
	testexec.Stub(t, map[string]string{"ansible": script})
	return log
}

func TestWaitSSHRunsWaitForConnection(t *testing.T) {
	log := stubAnsible(t, "ok", 0)
	if _, err := WaitSSH(Tunnel{}, "/tmp/inv.yml", 90*time.Second); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	got := strings.TrimSpace(string(b))
	want := "all --inventory /tmp/inv.yml --module-name wait_for_connection --args timeout=90"
	if got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestWaitSSHFailureCarriesOutput(t *testing.T) {
	stubAnsible(t, "UNREACHABLE: sshd not up", 4)
	out, err := WaitSSH(Tunnel{}, "/tmp/inv.yml", time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(out, "sshd not up") {
		t.Errorf("output = %q", out)
	}
}

func TestGrowDiskReturnsOutput(t *testing.T) {
	stubAnsible(t, "growpart: no space", 2)
	out, err := GrowDisk(Tunnel{}, "/tmp/inv.yml")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(out, "no space") {
		t.Errorf("output = %q", out)
	}
	stubAnsible(t, "CHANGED", 0)
	out, err = GrowDisk(Tunnel{}, "/tmp/inv.yml")
	if err != nil || !strings.Contains(out, "CHANGED") {
		t.Errorf("success: out=%q err=%v", out, err)
	}
}
