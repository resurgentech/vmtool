package testexec

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// emptyPath leaves only an empty directory on PATH, so a call these tests make
// can reach nothing real even if Stub is broken.
func emptyPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv(StubsEnv, "")
}

// run execs name only when it resolves inside dir.
func run(t *testing.T, dir, name string, args ...string) ([]byte, error) {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil || filepath.Dir(p) != dir {
		t.Fatalf("%s resolves to %q (%v), not inside the stub dir %q", name, p, err, dir)
	}
	return exec.Command(p, args...).CombinedOutput()
}

func TestStubInstallsExecutablesAndOptsIn(t *testing.T) {
	emptyPath(t)
	dir := Stub(t, map[string]string{"ansible": "#!/bin/sh\necho hi\n"})
	if dir == "" {
		t.Fatal("Stub returned no directory")
	}
	st, err := os.Stat(filepath.Join(dir, "ansible"))
	if err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("stub not executable: %v %v", st, err)
	}
	if got := os.Getenv(StubsEnv); got != dir {
		t.Errorf("%s = %q, want %q", StubsEnv, got, dir)
	}
	if !strings.HasPrefix(os.Getenv("PATH"), dir+string(os.PathListSeparator)) {
		t.Errorf("PATH = %q, want %q first", os.Getenv("PATH"), dir)
	}
	if p, err := exec.LookPath("ansible"); err != nil || filepath.Dir(p) != dir {
		t.Errorf("LookPath(ansible) = %q, %v", p, err)
	}
}

func TestAnsibleLogsCalls(t *testing.T) {
	emptyPath(t)
	log := Ansible(t)
	dir := os.Getenv(StubsEnv)
	for _, args := range [][]string{
		{"all", "--args", "growpart x"},
	} {
		if out, err := run(t, dir, "ansible", args...); err != nil {
			t.Fatalf("ansible %v: %v %s", args, err, out)
		}
	}
	if out, err := run(t, dir, "ansible-playbook", "pb.yml"); err != nil {
		t.Fatalf("ansible-playbook: %v %s", err, out)
	}
	if out, err := run(t, dir, "ssh", "-o", "X=1", "h", "true"); err != nil {
		t.Fatalf("ssh: %v %s", err, out)
	}
	if out, err := run(t, dir, "sshpass", "-e", "ssh", "h", "true"); err != nil {
		t.Fatalf("sshpass: %v %s", err, out)
	}
	t.Setenv("STUB_WAIT_FAIL", "1")
	out, err := run(t, dir, "ssh", "h", "true")
	if err == nil || !strings.Contains(string(out), "sshd not answering") {
		t.Fatalf("STUB_WAIT_FAIL: %v %s", err, out)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 5 || lines[0] != "grow" || lines[1] != "other pb.yml" || lines[2] != "probe -o X=1 h true" ||
		lines[3] != "probe h true" || lines[4] != "probe h true" {
		t.Fatalf("log = %q", lines)
	}
}
