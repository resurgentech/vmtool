package vmtool

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/testexec"
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
