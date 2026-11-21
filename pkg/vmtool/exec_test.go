package vmtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/testexec"
)

// (a) A test binary is never the tunnel.

func TestCurrentExeRefusesTestBinary(t *testing.T) {
	p, err := CurrentExe()
	if err == nil {
		t.Fatalf("CurrentExe() = %q under go test, want an error", p)
	}
	for _, want := range []string{"refusing to use the test binary", "Tunnel.Exe", "internal/testexec"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestTunnelCheckRefusesTestBinary(t *testing.T) {
	err := Tunnel{Exe: os.Args[0]}.check()
	if err == nil || !strings.Contains(err.Error(), "refusing to use the test binary") {
		t.Fatalf("check() with the test binary = %v", err)
	}
	// Any *.test name is refused too, wherever it lives.
	if err := (Tunnel{Exe: "/opt/x/server.test"}).check(); err == nil {
		t.Fatal("check() accepted a .test executable")
	}
	if err := (Tunnel{Exe: "/opt/vmtool/vmtool"}).check(); err != nil {
		t.Fatalf("check() refused a normal path: %v", err)
	}
}

func TestVMSSHRefusesTestBinaryTunnel(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := RemoteCmd(Tunnel{Exe: os.Args[0]}, "box", "web", []string{"true"}); err == nil ||
		!strings.Contains(err.Error(), "refusing to use the test binary") {
		t.Fatalf("RemoteCmd with the test binary as tunnel = %v", err)
	}
}

// (b) No real tool under go test without an explicit opt-in.

// sentinelPath puts a directory holding every guarded tool, each of which
// only touches a sentinel file, alone on PATH, and clears VMTOOL_TEST_STUBS.
// If any of them runs, the sentinel appears.
func sentinelPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "ran")
	for _, n := range []string{"ansible", "ansible-playbook", "ssh", "sshpass", "scp", "sudo", "true"} {
		body := "#!/bin/sh\necho " + n + " >> '" + sentinel + "'\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv(testexec.StubsEnv, "")
	return sentinel
}

func wantRefused(t *testing.T, what, tool string, err error) {
	t.Helper()
	want := `test tried to run real "` + tool + `"; install stubs with testexec.Stub(t, ...) (internal/testexec)`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: err = %v, want it to contain %q", what, err, want)
	}
}

func noSentinel(t *testing.T, sentinel string) {
	t.Helper()
	if b, err := os.ReadFile(sentinel); err == nil {
		t.Fatalf("a real tool was started: %q", b)
	}
}

func TestGuardRefusesRealToolsWithoutStub(t *testing.T) {
	sentinel := sentinelPath(t)

	_, err := GrowDisk(Tunnel{}, "/nonexistent/inv.yml")
	wantRefused(t, "GrowDisk", "ansible", err)

	_, err = RunPlaybook(Tunnel{}, "/nonexistent/inv.yml", "pb.yml", nil)
	wantRefused(t, "RunPlaybook", "ansible-playbook", err)

	err = sshCommand("192.0.2.1", Auth{User: "u", KeyPath: "/nonexistent/key"}, []string{"true"}).Run()
	wantRefused(t, "ssh", "ssh", err)

	err = sshCommand("192.0.2.1", Auth{User: "u", Password: "x"}, []string{"true"}).Run()
	wantRefused(t, "sshpass", "sshpass", err)

	// No name: no .machines lookup, DefaultAuth (a password), so sshpass.
	err = MachineRemoteCmd(Machine{Host: "192.0.2.1"}, 1, []string{"true"}).Run()
	wantRefused(t, "MachineRemoteCmd", "sshpass", err)

	err = sudoRun("x", "true")
	wantRefused(t, "sudoRun", "sudo", err)

	// A name nobody guards by hand is refused the same way.
	_, err = command("true")
	wantRefused(t, "command(true)", "true", err)

	noSentinel(t, sentinel)
}

// A stub directory opts in only the tools inside it: stubbing ansible does not
// let a real ssh through, even with the stub dir first on PATH.
func TestGuardAllowsOnlyTheStubDirectory(t *testing.T) {
	sentinel := sentinelPath(t)
	dir := testexec.Stub(t, map[string]string{"ansible": "#!/bin/sh\necho stubbed\n"})
	if _, err := command("ansible"); err != nil {
		t.Fatalf("stubbed ansible refused: %v", err)
	}
	_, err := command("ssh")
	wantRefused(t, "ssh beside an ansible stub", "ssh", err)
	// An absolute path outside the stub dir is refused as well.
	_, err = command("/bin/sh")
	wantRefused(t, "absolute /bin/sh", "/bin/sh", err)
	if !strings.HasPrefix(os.Getenv("PATH"), dir) {
		t.Fatalf("PATH = %q, want the stub dir first", os.Getenv("PATH"))
	}
	noSentinel(t, sentinel)
}

func TestGuardRunsInstalledStubs(t *testing.T) {
	sentinelPath(t)
	log := testexec.Ansible(t)
	if _, err := GrowDisk(Tunnel{}, "/nonexistent/inv.yml"); err != nil {
		t.Fatalf("GrowDisk with the stub: %v", err)
	}
	if _, err := RunPlaybook(Tunnel{}, "/nonexistent/inv.yml", "pb.yml", nil); err != nil {
		t.Fatalf("RunPlaybook with the stub: %v", err)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || lines[0] != "grow" || !strings.HasPrefix(lines[1], "other ") {
		t.Fatalf("stub calls = %q", lines)
	}

	out := filepath.Join(t.TempDir(), "argv")
	testexec.Stub(t, map[string]string{
		"ssh":     "#!/bin/sh\necho \"ssh $*\" >> '" + out + "'\n",
		"sshpass": "#!/bin/sh\necho \"sshpass $*\" >> '" + out + "'\n",
		"sudo":    "#!/bin/sh\necho \"sudo $*\" >> '" + out + "'\n",
	})
	if err := sshCommand("192.0.2.1", Auth{User: "u", KeyPath: "/k"}, []string{"true"}).Run(); err != nil {
		t.Fatalf("ssh stub: %v", err)
	}
	if err := sshCommand("192.0.2.1", Auth{User: "u", Password: "x"}, []string{"true"}).Run(); err != nil {
		t.Fatalf("sshpass stub: %v", err)
	}
	if err := sudoRun("x", "true"); err != nil {
		t.Fatalf("sudo stub: %v", err)
	}
	b, _ = os.ReadFile(out)
	got := string(b)
	for _, want := range []string{"ssh -o StrictHostKeyChecking=no", "sshpass -e ssh", "sudo -S true"} {
		if !strings.Contains(got, want) {
			t.Errorf("stub argv %q lacks %q", got, want)
		}
	}
}
