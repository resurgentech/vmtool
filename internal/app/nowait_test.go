package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// nwVMs records every call and fails WaitForIP with ipErr.
type nwVMs struct {
	createVMs
	ipErr   error
	calls   []string
	waitFor time.Duration
}

func (f *nwVMs) rec(s string) { f.calls = append(f.calls, s) }
func (f *nwVMs) CloneImage(b, n, p string, sz uint64) (string, error) {
	f.rec("CloneImage")
	return f.createVMs.CloneImage(b, n, p, sz)
}
func (f *nwVMs) Define(vmtool.VMConfig) error { f.rec("Define"); return nil }
func (f *nwVMs) Start(string) error           { f.rec("Start"); return nil }
func (f *nwVMs) ResizeVolume(string, uint64) error {
	f.rec("ResizeVolume")
	return nil
}
func (f *nwVMs) WaitForIP(_ string, d time.Duration) (string, error) {
	f.rec("WaitForIP")
	f.waitFor = d
	if f.ipErr != nil {
		return "", f.ipErr
	}
	return f.ip, nil
}
func (f *nwVMs) Destroy(string) error      { f.rec("Destroy"); return nil }
func (f *nwVMs) Delete(string, bool) error { f.rec("Delete"); return nil }
func (f *nwVMs) Reboot(string) error       { f.rec("Reboot"); return nil }
func (f *nwVMs) has(call string) bool {
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

var errNoIP = errors.New("timed out waiting for IP")

type nwRun struct {
	info *vmtool.VMInfo
	err  error
	evs  []ProgressEvent
	log  string // the stub ansible's call log
}

// nwCreate runs a create against m with stub ansible and a stub tunnel.
func nwCreate(t *testing.T, m *nwVMs, svc *Service, in CreateVMInput) nwRun {
	t.Helper()
	t.Chdir(t.TempDir())
	r := nwRun{log: sshStub(t)}
	in.Host, in.Name, in.Image = "jetfire", "web", "u.qcow2"
	in.OnProgress = func(ev ProgressEvent) { r.evs = append(r.evs, ev) }
	r.info, r.err = svc.WithTunnel(appTunnel()).CreateVM(context.Background(), m, in)
	return r
}

func (r nwRun) ansibleCalls() []string {
	b, _ := os.ReadFile(r.log)
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestCreateNoAddressIsAnErrorWithTheVM(t *testing.T) {
	for _, fw := range []string{"efi", "", "bios"} {
		m := &nwVMs{ipErr: errNoIP}
		r := nwCreate(t, m, &Service{IPTimeout: 2 * time.Minute}, CreateVMInput{Firmware: fw})
		if r.info == nil || r.info.Name != "web" {
			t.Fatalf("firmware %q: VM info not returned: %+v", fw, r.info)
		}
		ae := asAppErr(t, r.err)
		if ae.Kind != KindInternal || ae.Op != "wait_ip" {
			t.Errorf("firmware %q: kind %v op %q", fw, ae.Kind, ae.Op)
		}
		for _, want := range []string{"jetfire/web got no address within 2m0s", "(timed out waiting for IP)",
			"the VM exists and is running", "--no-wait"} {
			if !strings.Contains(r.err.Error(), want) {
				t.Errorf("firmware %q: error lacks %q: %v", fw, want, r.err)
			}
		}
		if hint := strings.Contains(r.err.Error(), vmtool.UEFIHint); hint != (fw != "bios") {
			t.Errorf("firmware %q: UEFI hint present = %v", fw, hint)
		}
		var evErr string
		for _, ev := range r.evs {
			if ev.Stage == "wait_ip" && ev.Status == "error" {
				evErr = ev.Detail
			}
		}
		if evErr == "" || !strings.Contains(r.err.Error(), evErr) {
			t.Errorf("firmware %q: wait_ip error event %q is not the error's text %q", fw, evErr, r.err)
		}
		if m.has("Destroy") || m.has("Delete") {
			t.Errorf("firmware %q: the VM was removed: %v", fw, m.calls)
		}
		if len(r.ansibleCalls()) != 0 {
			t.Errorf("firmware %q: ansible ran without an address: %v", fw, r.ansibleCalls())
		}
	}
}

func TestCreateNoAddressWithGrowOrPlaybookIsTheSameError(t *testing.T) {
	m := &nwVMs{ipErr: errNoIP}
	r := nwCreate(t, m, &Service{}, CreateVMInput{DiskSizeGB: 20})
	if ae := asAppErr(t, r.err); ae.Op != "wait_ip" || r.info == nil {
		t.Fatalf("op %q info %+v: %v", ae.Op, r.info, r.err)
	}
}

func TestCreateWaitForIPGetsServiceTimeout(t *testing.T) {
	m := &nwVMs{createVMs: createVMs{ip: "10.0.0.5"}}
	if r := nwCreate(t, m, &Service{IPTimeout: 37 * time.Second}, CreateVMInput{}); r.err != nil {
		t.Fatal(r.err)
	}
	if m.waitFor != 37*time.Second {
		t.Errorf("WaitForIP got %s, want 37s", m.waitFor)
	}
}

func TestCreateNoWaitReturnsAfterStart(t *testing.T) {
	m := &nwVMs{ipErr: errNoIP}
	r := nwCreate(t, m, &Service{InventoryPath: "inv.yml"}, CreateVMInput{NoWait: true})
	if r.err != nil || r.info == nil {
		t.Fatalf("NoWait: %+v, %v", r.info, r.err)
	}
	if m.has("WaitForIP") {
		t.Error("WaitForIP was called")
	}
	for _, ev := range r.evs {
		if ev.Stage == "wait_ip" || ev.Stage == "wait_ssh" {
			t.Errorf("unexpected event %+v", ev)
		}
	}
	if len(r.ansibleCalls()) != 0 {
		t.Errorf("ansible ran with --no-wait: %v", r.ansibleCalls())
	}
	if !exists("inv.yml") {
		t.Error("inventory not written with --no-wait")
	}
	if got := stageSeq(r.evs); !strings.HasSuffix(got, "start,write_inventory") {
		t.Errorf("stages = %s", got)
	}
}

func TestCreateInventoryWrittenBeforeAddressWait(t *testing.T) {
	m := &nwVMs{ipErr: errNoIP}
	r := nwCreate(t, m, &Service{InventoryPath: "inv.yml"}, CreateVMInput{})
	if r.err == nil {
		t.Fatal("want the no-address error")
	}
	if !exists("inv.yml") {
		t.Error("inventory not written on the no-address failure")
	}
	if got := stageSeq(r.evs); !strings.HasSuffix(got, "start,write_inventory,wait_ip") {
		t.Errorf("stages = %s, want start, write_inventory, wait_ip", got)
	}
}

func TestCreateStageOrder(t *testing.T) {
	m := &nwVMs{createVMs: createVMs{ip: "10.0.0.5"}}
	r := nwCreate(t, m, &Service{InventoryPath: "inv.yml"}, CreateVMInput{DiskSizeGB: 20})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := stageSeq(r.evs); !strings.HasSuffix(got, "start,write_inventory,wait_ip,wait_ssh,grow_disk") {
		t.Errorf("stages = %s", got)
	}
}

func TestCreateNoWaitWithPlaybookIsInvalid(t *testing.T) {
	m := &nwVMs{}
	r := nwCreate(t, m, &Service{}, CreateVMInput{NoWait: true, Playbook: "p.yml"})
	if ae := asAppErr(t, r.err); ae.Kind != KindInvalid {
		t.Fatalf("kind %v: %v", ae.Kind, r.err)
	}
	for _, want := range []string{"--no-wait", "--playbook"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, r.err)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("calls made before refusing: %v", m.calls)
	}
}

func TestCreateNoWaitWithDiskSizeResizesButDoesNotGrow(t *testing.T) {
	m := &nwVMs{}
	r := nwCreate(t, m, &Service{}, CreateVMInput{NoWait: true, DiskSizeGB: 20})
	if r.err != nil || r.info == nil {
		t.Fatalf("%+v, %v", r.info, r.err)
	}
	if !m.has("ResizeVolume") || m.has("WaitForIP") {
		t.Errorf("calls = %v", m.calls)
	}
	if len(r.ansibleCalls()) != 0 {
		t.Errorf("ansible ran: %v", r.ansibleCalls())
	}
	var skip bool
	for _, ev := range r.evs {
		if ev.Stage == "grow_disk" && strings.Contains(ev.Detail, "not growing the filesystem (--no-wait); cloud-init grows it at first boot on images built since C3") {
			skip = true
		}
		if ev.Stage == "wait_ssh" {
			t.Errorf("wait_ssh event: %+v", ev)
		}
	}
	if !skip {
		t.Errorf("no grow_disk skip event: %+v", r.evs)
	}
}

func TestCreateWaitsForSSHOnEveryCreate(t *testing.T) {
	m := &nwVMs{createVMs: createVMs{ip: "10.0.0.5"}}
	r := nwCreate(t, m, &Service{}, CreateVMInput{})
	if r.err != nil {
		t.Fatal(r.err)
	}
	calls := r.ansibleCalls()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "wait ") {
		t.Fatalf("ansible calls = %q, want one wait_for_connection", calls)
	}
}

func TestCreateWaitSSHFailureLeavesVM(t *testing.T) {
	t.Setenv("STUB_WAIT_FAIL", "1")
	m := &nwVMs{createVMs: createVMs{ip: "10.0.0.5"}}
	r := nwCreate(t, m, &Service{SSHTimeout: 2 * time.Minute}, CreateVMInput{})
	if r.info == nil || r.info.Name != "web" {
		t.Fatalf("VM info not returned: %+v", r.info)
	}
	ae := asAppErr(t, r.err)
	if ae.Kind != KindInternal || ae.Op != "wait_ssh" || !strings.Contains(ae.Output, "sshd not answering") {
		t.Fatalf("kind %v op %q output %q", ae.Kind, ae.Op, ae.Output)
	}
	for _, want := range []string{"jetfire/web did not accept SSH within 2m0s", "the VM exists and is running", "--no-wait"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, r.err)
		}
	}
	if m.has("Destroy") || m.has("Delete") {
		t.Errorf("the VM was removed: %v", m.calls)
	}
}

func TestCreateGrowAndPlaybookShareOneWait(t *testing.T) {
	pb := t.TempDir() + "/p.yml"
	if err := os.WriteFile(pb, []byte("- hosts: all\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &nwVMs{createVMs: createVMs{ip: "10.0.0.5"}}
	r := nwCreate(t, m, &Service{}, CreateVMInput{DiskSizeGB: 20, Playbook: pb})
	if r.err != nil {
		t.Fatal(r.err)
	}
	var waits int
	for _, c := range r.ansibleCalls() {
		if strings.HasPrefix(c, "wait ") {
			waits++
		}
	}
	if waits != 1 {
		t.Errorf("%d waits, want 1: %q", waits, r.ansibleCalls())
	}
}
