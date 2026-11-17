package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// cappedEnv is a PATH holding only stub tools: `go` records its argv, and
// with withSystemdRun a `systemd-run` records its argv, answers the probe
// (`--user --scope -q true`) and otherwise execs what follows `--`, as the
// real one does. getconf (the CPU count) prints 16. Nothing real can be
// reached from it.
type cappedEnv struct {
	bin, log string
}

func newCappedEnv(t *testing.T, withSystemdRun bool) *cappedEnv {
	t.Helper()
	e := &cappedEnv{bin: t.TempDir(), log: filepath.Join(t.TempDir(), "argv.log")}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(e.bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("go", `echo "go $*" >> '`+e.log+`'`+"\n")
	stub("getconf", "echo 16\n")
	if withSystemdRun {
		stub("systemd-run", `if [ "$*" = "--user --scope -q true" ]; then exit 0; fi
echo "systemd-run $*" >> '`+e.log+`'
while [ $# -gt 0 ]; do a=$1; shift; [ "$a" = "--" ] && exec "$@"; done
exit 1
`)
	}
	return e
}

// run runs scripts/test-capped.sh under bash (resolved before PATH is cut
// down) with only the stub dir on PATH.
func (e *cappedEnv) run(t *testing.T, env []string, args ...string) (string, string, int) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	script, err := filepath.Abs("test-capped.sh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, append([]string{script}, args...)...)
	cmd.Env = append([]string{"PATH=" + e.bin, "HOME=" + t.TempDir()}, env...)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.log)
	return string(b), se.String(), code
}

func TestTestCappedUsesSystemdScope(t *testing.T) {
	e := newCappedEnv(t, true)
	calls, stderr, code := e.run(t, nil)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	// The CPU count comes from getconf, not nproc: inside a scope, nproc
	// reports the cgroup's CPU quota, not the machine's cores. The scope has
	// its own unit name: systemd-run's default is derived from the PID, which
	// an exec chain (systemd-run -> this script -> systemd-run) keeps, so the
	// default name collides with the outer scope.
	re := regexp.MustCompile(`(?m)^systemd-run --user --scope -q --unit=vmtool-test-[0-9]+-[0-9]+ -p TasksMax=[1-9][0-9]* -p CPUQuota=800% -- go test \./\.\.\.$`)
	if !re.MatchString(calls) {
		t.Errorf("systemd-run argv not as expected:\n%s", calls)
	}
	if !strings.Contains(calls, "\ngo test ./...\n") {
		t.Errorf("go test ./... did not run inside the scope:\n%s", calls)
	}
	if strings.Contains(stderr, "without a resource cap") {
		t.Errorf("fallback warning with systemd-run present: %q", stderr)
	}
}

func TestTestCappedPassesArgsAndOverrides(t *testing.T) {
	e := newCappedEnv(t, true)
	calls, stderr, code := e.run(t, []string{"VMTOOL_TEST_TASKS_MAX=77", "VMTOOL_TEST_CPU_QUOTA=150%"}, "-run", "TestX", "./pkg/...")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	want := regexp.MustCompile(`(?m)^systemd-run --user --scope -q --unit=vmtool-test-[0-9]+-[0-9]+ -p TasksMax=77 -p CPUQuota=150% -- go test -run TestX \./pkg/\.\.\.$`)
	if !want.MatchString(calls) {
		t.Errorf("calls = %q, want %s", calls, want)
	}
}

func TestTestCappedFallsBackWithoutSystemdRun(t *testing.T) {
	e := newCappedEnv(t, false)
	calls, stderr, code := e.run(t, nil)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stderr, "test-capped: systemd-run --user not available; running go test without a resource cap") {
		t.Errorf("stderr = %q", stderr)
	}
	if calls != "go test ./...\n" {
		t.Errorf("calls = %q, want plain go test ./...", calls)
	}
}

// The probe can fail (no user manager, e.g. in a container): fall back too.
func TestTestCappedFallsBackWhenProbeFails(t *testing.T) {
	e := newCappedEnv(t, false)
	if err := os.WriteFile(filepath.Join(e.bin, "systemd-run"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls, stderr, _ := e.run(t, nil)
	if !strings.Contains(stderr, "without a resource cap") || calls != "go test ./...\n" {
		t.Errorf("stderr = %q, calls = %q", stderr, calls)
	}
}

// With systemd-run working, an error while building the capped command ends
// the script: it never falls through to an uncapped go test.
func TestTestCappedNeverFallsBackOnError(t *testing.T) {
	e := newCappedEnv(t, true)
	if err := os.WriteFile(filepath.Join(e.bin, "getconf"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls, stderr, code := e.run(t, nil)
	if code == 0 || calls != "" || strings.Contains(stderr, "without a resource cap") {
		t.Errorf("exit %d, calls %q, stderr %q: want a failure and no go test", code, calls, stderr)
	}
}

// The property really takes effect: one bounded process in a real scope reads
// its own pids.max. Skipped where systemd-run --user does not work.
func TestTestCappedRealScopeHonoursTasksMax(t *testing.T) {
	sr, err := exec.LookPath("systemd-run")
	if err != nil {
		t.Skip("no systemd-run")
	}
	if exec.Command(sr, "--user", "--scope", "-q", "true").Run() != nil {
		t.Skip("systemd-run --user does not work here")
	}
	out, err := exec.Command(sr, "--user", "--scope", "-q", "-p", "TasksMax=7", "--",
		"sh", "-c", "cat /sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/pids.max").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "7" {
		t.Fatalf("pids.max = %q, want 7", got)
	}
}

// `just test` (and so `just check` and the orch commit hook) goes through the
// script.
func TestJustTestRunsCapped(t *testing.T) {
	b, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^test[^:\n]*:\n((?:[ \t]+.*\n)+)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no test recipe in justfile")
	}
	body := string(m[1])
	if !strings.Contains(body, "scripts/test-capped.sh") || regexp.MustCompile(`(?m)^\s*(cd .*&& )?go test`).MatchString(body) {
		t.Errorf("test recipe does not run scripts/test-capped.sh only:\n%s", body)
	}
	if !regexp.MustCompile(`(?m)^check:.*\btest\b`).Match(b) {
		t.Error("check does not depend on test")
	}
}
