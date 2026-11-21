package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

func fanOut(rows ...app.HostVMs) func(context.Context) []app.HostVMs {
	return func(context.Context) []app.HostVMs { return rows }
}

// The refresh command runs the fan-out and carries every host's rows, a dead
// host as an error row; the local Manager is not asked to list.
func TestRefreshUsesFanOut(t *testing.T) {
	local := &tuiFake{host: "jetfire"}
	m := initialModel(local, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	m.hostNames = []string{"jetfire", "veritech01", "veritech02"}
	m.listAll = fanOut(
		app.HostVMs{Host: "jetfire", VMs: []vmtool.VMInfo{{Name: "web", State: vmtool.StateRunning}}},
		app.HostVMs{Host: "veritech01", Err: errors.New("timed out after 5s")},
		app.HostVMs{Host: "veritech02", VMs: []vmtool.VMInfo{{Name: "db", State: vmtool.StateRunning, IP: "10.0.0.9"}}},
	)
	msg, ok := m.refreshCmd()().(refreshMsg)
	if !ok {
		t.Fatal("refresh did not produce a refreshMsg")
	}
	if local.listed != 0 {
		t.Errorf("local manager listed %d times; the fan-out owns listing", local.listed)
	}
	next, _ := m.Update(msg)
	m = next.(model)
	if len(m.vms) != 3 {
		t.Fatalf("rows %+v", m.vms)
	}
	out := m.View()
	for _, want := range []string{"jetfire/web", "veritech02/db", "veritech01", "ERROR: timed out after 5s"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	// Each row acts on its own host; an error row has nothing to act on.
	m.cursor = 2
	vm, ok := m.selectedVM()
	if !ok || m.vmRef(vm) != (app.VMRef{Host: "veritech02", Name: "db"}) {
		t.Errorf("row 2 selected %+v ok=%v", vm, ok)
	}
	m.cursor = 1
	if _, ok := m.selectedVM(); ok {
		t.Error("an error row is selectable as a VM")
	}
}

// A dead host costs the fan-out its timeout, not the UI: refresh is a
// command (the program runs it off the update loop), and through the real
// fan-out a blocked host is an error row after the timeout while the others
// answer.
func TestRefreshDoesNotWaitOnDeadHost(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	local := &tuiFake{host: "jetfire"}
	m := initialModel(local, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	m.hostNames = []string{"jetfire", "dead"}
	m.resolve = func(host string) (app.Manager, func(), error) {
		if host == "dead" {
			return &hangFake{block: block}, func() {}, nil
		}
		return local, func() {}, nil
	}
	m.listAll = func(ctx context.Context) []app.HostVMs {
		return app.ListAll(ctx, m.hostNames, m.resolve, 200*time.Millisecond)
	}
	start := time.Now()
	msg := m.refreshCmd()().(refreshMsg)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("refresh took %v", time.Since(start))
	}
	if len(msg.vms) != 2 || msg.vms[0].Name != "jetfire-vm" || msg.vms[1].Host != "dead" || msg.vms[1].Err == nil {
		t.Fatalf("rows %+v", msg.vms)
	}
}

type hangFake struct {
	app.Manager
	block chan struct{}
}

func (h *hangFake) List() ([]vmtool.VMInfo, error) { <-h.block; return nil, nil }

// After an action the rows come from the fan-out too, not the local host
// alone.
func TestActionRefreshUsesFanOut(t *testing.T) {
	local := &tuiFake{host: "jetfire"}
	other := &tuiFake{host: "veritech01"}
	m := initialModel(local, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	m.resolve = func(host string) (app.Manager, func(), error) { return other, func() {}, nil }
	m.listAll = fanOut(
		app.HostVMs{Host: "jetfire", VMs: []vmtool.VMInfo{{Name: "a"}}},
		app.HostVMs{Host: "veritech01", VMs: []vmtool.VMInfo{{Name: "b"}}},
	)
	ref := app.VMRef{Host: "veritech01", Name: "b"}
	_, cmd := m.runLifecycle("start", ref, func(svc *app.Service, mgr app.Manager) error {
		_, err := svc.StartVM(context.Background(), mgr, ref)
		return err
	})
	msg := cmd().(cmdDoneMsg)
	if len(msg.vms) != 2 || msg.vms[1].Host != "veritech01" || local.listed != 0 {
		t.Fatalf("rows %+v (local listed %d)", msg.vms, local.listed)
	}
}
