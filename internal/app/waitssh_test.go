package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sshStub installs stub `ansible` and `ansible-playbook` executables. Each
// invocation appends "<tool> <module-or-playbook>" to the returned log.
// STUB_WAIT_FAIL / STUB_GROW_FAIL make the matching call exit 4 / 2 with
// output; STUB_GROW_OUT is what a failing grow prints.
func sshStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
case "$*" in
*wait_for_connection*)
  echo "wait $*" >> '` + log + `'
  if [ -n "$STUB_WAIT_FAIL" ]; then echo "wait boom: sshd not answering"; exit 4; fi ;;
*growpart*)
  echo "grow" >> '` + log + `'
  if [ -n "$STUB_GROW_FAIL" ]; then echo "$STUB_GROW_OUT"; exit 2; fi ;;
*)
  echo "other $*" >> '` + log + `' ;;
esac
exit 0
`
	for _, n := range []string{"ansible", "ansible-playbook"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

type sshVMs struct{ createVMs }

func (f *sshVMs) ResizeVolume(string, uint64) error { return nil }
func (f *sshVMs) Reboot(string) error               { return nil }

func sshCreate(t *testing.T, svc *Service, in CreateVMInput) (error, []ProgressEvent) {
	t.Helper()
	t.Chdir(t.TempDir())
	m := &sshVMs{createVMs{ip: "10.0.0.5"}}
	var evs []ProgressEvent
	in.Host, in.Name, in.Image = "jetfire", "web", "u.qcow2"
	in.OnProgress = func(ev ProgressEvent) { evs = append(evs, ev) }
	_, err := svc.WithTunnel(appTunnel()).CreateVM(context.Background(), m, in)
	return err, evs
}

func stageSeq(evs []ProgressEvent) string {
	var s []string
	for _, ev := range evs {
		if ev.Status == "start" {
			s = append(s, ev.Stage)
		}
	}
	return strings.Join(s, ",")
}

func TestCreateWaitsForSSHBeforeGrow(t *testing.T) {
	log := sshStub(t)
	err, evs := sshCreate(t, &Service{SSHTimeout: 45 * time.Second}, CreateVMInput{DiskSizeGB: 20})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "wait ") || !strings.Contains(lines[0], "timeout=45") || lines[1] != "grow" {
		t.Fatalf("calls = %q", lines)
	}
	if got := stageSeq(evs); !strings.Contains(got, "wait_ip,wait_ssh") || !strings.HasSuffix(got, "wait_ssh,grow_disk") {
		t.Errorf("stages = %s, want wait_ip then wait_ssh then grow_disk", got)
	}
}

func TestCreateDefaultSSHTimeout120(t *testing.T) {
	log := sshStub(t)
	if err, _ := sshCreate(t, &Service{}, CreateVMInput{DiskSizeGB: 20}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "timeout=120") {
		t.Fatalf("calls = %q", b)
	}
}

func TestCreateWaitSSHFailureNamesVMAndWait(t *testing.T) {
	log := sshStub(t)
	t.Setenv("STUB_WAIT_FAIL", "1")
	err, evs := sshCreate(t, &Service{SSHTimeout: 30 * time.Second}, CreateVMInput{DiskSizeGB: 20})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"web", "SSH", "30s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	var ae *Error
	if !AsError(err, &ae) || !strings.Contains(ae.Output, "sshd not answering") {
		t.Errorf("Error.Output = %#v", ae)
	}
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "grow") {
		t.Errorf("grow ran after failed wait: %q", b)
	}
	var sawErr bool
	for _, ev := range evs {
		if ev.Stage == "wait_ssh" && ev.Status == "error" {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("no wait_ssh error event: %+v", evs)
	}
}

func TestCreateGrowFailureCarriesOutputNotPassword(t *testing.T) {
	log := sshStub(t)
	t.Setenv("STUB_GROW_FAIL", "1")
	t.Setenv("STUB_GROW_OUT", "growpart: NOCHANGE for hunter2pw")
	err, _ := sshCreate(t, &Service{}, CreateVMInput{DiskSizeGB: 20, SSHPass: "hunter2pw"})
	if err == nil {
		t.Fatal("expected error")
	}
	var ae *Error
	if !AsError(err, &ae) || !strings.Contains(ae.Output, "NOCHANGE") {
		t.Fatalf("Error.Output = %#v", ae)
	}
	if strings.Contains(ae.Output, "hunter2pw") || strings.Contains(err.Error(), "hunter2pw") {
		t.Errorf("password leaked: %q / %q", ae.Output, err)
	}
	b, _ := os.ReadFile(log)
	if n := strings.Count(string(b), "grow"); n != 1 {
		t.Errorf("grow ran %d times, want 1 (no retry)", n)
	}
}

func TestCreateWithoutGrowOrPlaybookSkipsWaitSSH(t *testing.T) {
	log := sshStub(t)
	err, evs := sshCreate(t, &Service{}, CreateVMInput{})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("ansible ran: %q", b)
	}
	if strings.Contains(stageSeq(evs), "wait_ssh") {
		t.Errorf("stages = %s", stageSeq(evs))
	}
}

func TestCreateWaitsForSSHBeforePlaybook(t *testing.T) {
	log := sshStub(t)
	pb := filepath.Join(t.TempDir(), "p.yml")
	if err := os.WriteFile(pb, []byte("- hosts: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err, evs := sshCreate(t, &Service{}, CreateVMInput{Playbook: pb})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "wait ") || !strings.HasPrefix(lines[1], "other ") {
		t.Fatalf("calls = %q", lines)
	}
	if got := stageSeq(evs); !strings.Contains(got, "wait_ip,wait_ssh") {
		t.Errorf("stages = %s", got)
	}
}
