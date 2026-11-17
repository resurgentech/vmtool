package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

func TestParseProbe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		out    string
		compat bool
		sudo   bool
	}{
		{"ubuntu", "ID=ubuntu\nID_LIKE=debian\nPRETTY_NAME=Ubuntu 24.04.1 LTS\nSUDO=ok\n", true, true},
		{"debian no sudo", "ID=debian\nID_LIKE=\nPRETTY_NAME=Debian GNU/Linux 12\n", true, false},
		{"mint", "ID=linuxmint\nID_LIKE=ubuntu debian\nPRETTY_NAME=Linux Mint 22\nSUDO=ok\n", true, true},
		{"fedora", "ID=fedora\nID_LIKE=\nPRETTY_NAME=Fedora Linux 41\nSUDO=ok\n", false, true},
		{"arch", "ID=arch\nID_LIKE=\nPRETTY_NAME=Arch Linux\nSUDO=ok\n", false, true},
		{"empty", "", false, false},
	} {
		r := parseProbe(tc.out)
		if r.ubuntuCompatible() != tc.compat || r.Sudo != tc.sudo {
			t.Errorf("%s: %+v compat=%v", tc.name, r, r.ubuntuCompatible())
		}
	}
	if r := parseProbe("PRETTY_NAME=Ubuntu 24.04.1 LTS\n"); r.Pretty != "Ubuntu 24.04.1 LTS" {
		t.Errorf("pretty = %q", r.Pretty)
	}
}

func TestRunRemoteExitCodes(t *testing.T) {
	res, err := runRemote("t", exec.Command("sh", "-c", "echo out; echo err >&2; exit 3"), 0)
	if err != nil || res.ExitCode != 3 || res.Stdout != "out\n" || res.Stderr != "err\n" {
		t.Fatalf("exit 3: %+v %v", res, err)
	}
	// 255 is a result for user commands (VM/machine cmd contract)...
	res, err = runRemote("t", exec.Command("sh", "-c", "echo boom >&2; exit 255"), 0)
	if err != nil || res.ExitCode != 255 {
		t.Fatalf("exit 255: %+v %v", res, err)
	}
	// ...and an ssh failure for commands vmtool chose.
	if ae := asAppErr(t, sshFailed("t", res)); ae.Kind != KindBadGateway {
		t.Fatalf("sshFailed kind %v", ae.Kind)
	}
	if err := sshFailed("t", &CmdResult{ExitCode: 1}); err != nil {
		t.Fatalf("sshFailed(1) = %v", err)
	}
	start := time.Now()
	_, err = runRemote("t", exec.Command("sleep", "10"), 100*time.Millisecond)
	if ae := asAppErr(t, err); ae.Kind != KindBadGateway || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
	_, err = runRemote("t", exec.Command("/nonexistent/ssh"), 0)
	if ae := asAppErr(t, err); ae.Kind != KindBadGateway {
		t.Fatalf("start failure kind %v", ae.Kind)
	}
}

// fakeVMs is a Manager that only answers Info.
type fakeVMs struct {
	Manager
	names map[string]bool
}

func (f fakeVMs) Info(name string) (*vmtool.VMInfo, error) {
	if f.names[name] {
		return &vmtool.VMInfo{Name: name}, nil
	}
	return nil, fmt.Errorf("looking up domain %q: %w", name, vmtool.ErrNotFound)
}

func TestAddMachineRejectsBeforeSSH(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Kind: vmtool.MachineMetal, Host: "10.0.0.42"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".machines", "oldvm"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".machines", "oldvm", "password.yml"), []byte("username: bot\npassword: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vms := fakeVMs{names: map[string]bool{"web": true}}

	for _, tc := range []struct {
		name string
		in   AddMachineInput
		kind Kind
	}{
		{"bad name", AddMachineInput{Name: "../x", Host: "h"}, KindInvalid},
		{"no host", AddMachineInput{Name: "a"}, KindInvalid},
		{"bad kind", AddMachineInput{Name: "a", Host: "h", Kind: "vm"}, KindInvalid},
		{"commission metal", AddMachineInput{Name: "a", Host: "h", Commission: true}, KindInvalid},
		{"libvirt VM name", AddMachineInput{Name: "web", Host: "h"}, KindConflict},
		{"other host", AddMachineInput{Name: "nuc1", Host: "10.0.0.99"}, KindConflict},
		{"other kind", AddMachineInput{Name: "nuc1", Host: "10.0.0.42", Kind: "alien"}, KindConflict},
		{"VM creds dir", AddMachineInput{Name: "oldvm", Host: "h"}, KindConflict},
		{"VM SSH name", AddMachineInput{Name: "x.jetfire.vmtool", Host: "h"}, KindInvalid},
		{"VM SSH name, any case", AddMachineInput{Name: "X.VMTOOL", Host: "h"}, KindInvalid},
		{"local host name", AddMachineInput{Name: "jetfire", Host: "h"}, KindConflict},
		{"configured host name", AddMachineInput{Name: "veritech01", Host: "h"}, KindConflict},
	} {
		_, err := (&Service{HostNames: []string{"jetfire", "veritech01"}}).AddMachine(context.Background(), vms, tc.in)
		if ae := asAppErr(t, err); ae.Kind != tc.kind {
			t.Errorf("%s: kind %v, want %v (%v)", tc.name, ae.Kind, tc.kind, err)
		}
	}
	if !vmtool.HasCredentials("oldvm") {
		t.Fatal("AddMachine touched existing creds")
	}
}

func TestMachinePlaybookGuards(t *testing.T) {
	t.Chdir(t.TempDir())
	s := &Service{PlaybookDir: "ansible/playbooks"}
	_, err := s.RunMachinePlaybook(context.Background(), "nuc1", "commission.yml", nil)
	if ae := asAppErr(t, err); ae.Kind != KindInvalid {
		t.Fatalf("commission: %v", err)
	}
	_, err = s.RunMachinePlaybook(context.Background(), "nuc1", "ansible/playbooks/commission.yml", nil)
	if ae := asAppErr(t, err); ae.Kind != KindInvalid {
		t.Fatalf("commission path: %v", err)
	}
	_, err = s.RunMachinePlaybook(context.Background(), "nuc1", "vm_upgrade.yml", nil)
	if ae := asAppErr(t, err); ae.Kind != KindNotFound {
		t.Fatalf("unknown machine: %v", err)
	}
	for _, op := range []func() error{
		func() error { _, err := s.GetMachine(context.Background(), "nuc1"); return err },
		func() error { _, err := s.RebootMachine(context.Background(), "nuc1"); return err },
		func() error {
			_, err := s.RunMachineCommand(context.Background(), "nuc1", []string{"true"})
			return err
		},
		func() error { _, err := s.MachineSSHCmd("nuc1"); return err },
		func() error { return s.RemoveMachine(context.Background(), "nuc1") },
	} {
		if ae := asAppErr(t, op()); ae.Kind != KindNotFound {
			t.Errorf("unknown machine kind %v", ae.Kind)
		}
	}
}

func TestWithMachineVars(t *testing.T) {
	m := vmtool.Machine{Name: "cloud1", Kind: vmtool.MachineAlien}
	got := withMachineVars(m, nil)
	if got["deviceid"] != "cloud1" || got["vmtool_kind"] != "alien" {
		t.Fatalf("defaults: %v", got)
	}
	got = withMachineVars(m, map[string]string{"vmtool_kind": "vm", "x": "y"})
	if got["vmtool_kind"] != "vm" || got["x"] != "y" {
		t.Fatalf("override: %v", got)
	}
}

func TestCreateVMRefusesMachineName(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Kind: vmtool.MachineMetal, Host: "10.0.0.42"}); err != nil {
		t.Fatal(err)
	}
	_, err := (&Service{}).CreateVM(context.Background(), nil, CreateVMInput{Host: "jetfire", Name: "nuc1", Image: "ubuntu.qcow2"})
	if ae := asAppErr(t, err); ae.Kind != KindConflict {
		t.Fatalf("kind %v: %v", ae.Kind, err)
	}
}

func asAppErr(t *testing.T, err error) *Error {
	t.Helper()
	var ae *Error
	if !AsError(err, &ae) {
		t.Fatalf("want *app.Error, got %#v", err)
	}
	return ae
}
