package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// Rows name VMs host/vm; machines keep their bare names.
func TestRowsShowHostSlashVM(t *testing.T) {
	m := initialModel(nil, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	m.vms = []vmRow{{Host: "jetfire", VMInfo: vmtool.VMInfo{Name: "web", State: vmtool.StateRunning, IP: "10.0.0.5"}}}
	out := m.View()
	if !strings.Contains(out, "jetfire/web") {
		t.Fatalf("view has no jetfire/web:\n%s", out)
	}
	if ref := m.vmRef(m.vms[0]); ref.Host != "jetfire" || ref.Name != "web" {
		t.Fatalf("vmRef = %+v", ref)
	}
}

// tuiFake is the part of app.Manager the lifecycle path uses; anything else
// panics (the embedded interface is nil).
type tuiFake struct {
	app.Manager
	host    string
	started []string
	listed  int
}

func (f *tuiFake) Start(n string) error { f.started = append(f.started, n); return nil }
func (f *tuiFake) Info(n string) (*vmtool.VMInfo, error) {
	return &vmtool.VMInfo{Name: n, State: vmtool.StateRunning}, nil
}
func (f *tuiFake) List() ([]vmtool.VMInfo, error) {
	f.listed++
	return []vmtool.VMInfo{{Name: f.host + "-vm"}}, nil
}

// An action on a VM ref goes through the host resolver for the ref's host,
// not a hard-wired local manager; the row list still comes from the local
// host.
func TestActionsGoThroughResolver(t *testing.T) {
	local := &tuiFake{host: "jetfire"}
	other := &tuiFake{host: "veritech01"}
	var asked []string
	released := 0
	m := initialModel(local, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	m.resolve = func(host string) (app.Manager, func(), error) {
		asked = append(asked, host)
		if host == "veritech01" {
			return other, func() { released++ }, nil
		}
		return nil, nil, errors.New("unexpected host " + host)
	}
	ref := app.VMRef{Host: "veritech01", Name: "web"}
	_, cmd := m.runLifecycle("start", ref, func(svc *app.Service, mgr app.Manager) error {
		_, err := svc.StartVM(context.Background(), mgr, ref)
		return err
	})
	msg, ok := cmd().(cmdDoneMsg)
	if !ok || msg.status != logStatusDone {
		t.Fatalf("msg = %+v", msg)
	}
	if len(asked) != 1 || asked[0] != "veritech01" || released != 1 {
		t.Fatalf("resolver asked %v, released %d", asked, released)
	}
	if len(other.started) != 1 || other.started[0] != "web" || len(local.started) != 0 {
		t.Fatalf("started: veritech01 %v, local %v", other.started, local.started)
	}
	if local.listed != 1 || len(msg.vms) != 1 || msg.vms[0].Name != "jetfire-vm" {
		t.Fatalf("rows came from %+v (local listed %d)", msg.vms, local.listed)
	}

	// A host the resolver refuses is an error line, not a panic.
	_, cmd = m.runLifecycle("start", app.VMRef{Host: "nosuch", Name: "web"}, func(svc *app.Service, mgr app.Manager) error {
		t.Error("ran without a manager")
		return nil
	})
	if msg := cmd().(cmdDoneMsg); msg.status != logStatusError || !strings.Contains(msg.title, "unexpected host nosuch") {
		t.Fatalf("msg = %+v", msg)
	}
}

// The TUI's service carries the tunnel the CLI handed it, so its ssh and
// inventory writes go through internal/app with the right token.
func TestSvcCarriesTunnel(t *testing.T) {
	m := initialModel(nil, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	m.tunnel = vmtool.Tunnel{Exe: "/opt/vmtool/vmtool", Token: "tui-test-token"}
	if got := m.svc().Tunnel; got != m.tunnel {
		t.Fatalf("svc tunnel = %+v", got)
	}
}
