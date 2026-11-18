package app

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const evPlaceholder = "PLACEHOLDER-not-a-secret-1"

func TestCreateVMBadExtraVarNameIsInvalidBeforeLibvirt(t *testing.T) {
	m := &nwVMs{}
	r := nwCreate(t, m, &Service{}, CreateVMInput{Playbook: "x.yml", ExtraVars: map[string]string{"foo-bar": evPlaceholder}})
	if ae := asAppErr(t, r.err); ae.Kind != KindInvalid {
		t.Fatalf("kind %v: %v", ae.Kind, r.err)
	}
	if strings.Contains(r.err.Error(), evPlaceholder) {
		t.Fatalf("value in error: %v", r.err)
	}
	if len(m.calls) != 0 {
		t.Fatalf("libvirt calls made: %v", m.calls)
	}
}

func TestRunPlaybookBadExtraVarNameIsInvalid(t *testing.T) {
	t.Chdir(t.TempDir())
	log := sshStub(t)
	s := (&Service{PlaybookDir: "pb"}).WithTunnel(appTunnel())
	_, err := s.RunPlaybook(context.Background(), &createVMs{ip: "10.0.0.5"}, VMRef{Host: "jetfire", Name: "web"},
		"x.yml", vmtool.Auth{}, map[string]string{"foo-bar": evPlaceholder})
	if ae := asAppErr(t, err); ae.Kind != KindInvalid {
		t.Fatalf("kind %v: %v", ae.Kind, err)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Fatalf("ansible ran: %s", b)
	}
}

func TestRunMachinePlaybookBadExtraVarNameIsInvalid(t *testing.T) {
	t.Chdir(t.TempDir())
	log := sshStub(t)
	_, err := (&Service{PlaybookDir: "pb"}).RunMachinePlaybook(context.Background(), "nuc1", "x.yml",
		map[string]string{"1a": evPlaceholder})
	if ae := asAppErr(t, err); ae.Kind != KindInvalid {
		t.Fatalf("kind %v: %v", ae.Kind, err)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Fatalf("ansible ran: %s", b)
	}
}

func TestPlaybookRunsKeepValuesOutOfArgv(t *testing.T) {
	t.Chdir(t.TempDir())
	log := sshStub(t)
	ctx := context.Background()
	s := (&Service{PlaybookDir: "pb"}).WithTunnel(appTunnel())
	if _, err := s.RunPlaybook(ctx, &createVMs{ip: "10.0.0.5"}, VMRef{Host: "jetfire", Name: "webvm"},
		"x.yml", vmtool.Auth{}, map[string]string{"k1": evPlaceholder}); err != nil {
		t.Fatal(err)
	}
	if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Host: "10.0.0.9", Kind: vmtool.MachineMetal}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunMachinePlaybook(ctx, "nuc1", "x.yml", map[string]string{"k1": evPlaceholder}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("calls: %q", lines)
	}
	for _, l := range lines {
		for _, bad := range []string{evPlaceholder, "webvm", "=metal", "=nuc1"} {
			if strings.Contains(l, bad) {
				t.Errorf("call %q holds %q", l, bad)
			}
		}
		if !strings.Contains(l, `deviceid={{ lookup("ansible.builtin.env", "VMTOOL_VAR_deviceid") }}`) {
			t.Errorf("call %q lacks the deviceid lookup", l)
		}
	}
	if !strings.Contains(lines[1], "VMTOOL_VAR_vmtool_kind") {
		t.Errorf("machine call lacks the vmtool_kind lookup: %q", lines[1])
	}
}
