package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// V6: the TUI lists a network home (a host that runs no VMs) as one row that
// says so, which is not a VM to act on; create refuses it through the same
// service the other surfaces use.
func TestTUIShowsNetworkHome(t *testing.T) {
	r := hosts.NewWith("ws", config.RoleWorkstation, []string{"hv1", "home1"}, func() (app.Manager, func(), error) {
		return &tuiFake{host: "ws"}, func() {}, nil
	}).WithNetworkHomes("home1")
	m := newModel(r, vmtool.Tunnel{}, 0, 0)
	m.listAll = fanOut(
		app.HostVMs{Host: "hv1", VMs: []vmtool.VMInfo{{Name: "db", State: vmtool.StateRunning}}},
		app.HostVMs{Host: "home1"},
	)
	msg, ok := m.refreshCmd()().(refreshMsg)
	if !ok {
		t.Fatal("refresh did not produce a refreshMsg")
	}
	next, _ := m.Update(msg)
	m = next.(model)
	out := m.View()
	if !strings.Contains(out, "home1") || !strings.Contains(out, "network home") {
		t.Errorf("view lacks the network home row:\n%s", out)
	}
	if strings.Contains(out, "ERROR") {
		t.Errorf("a network home is shown as an error:\n%s", out)
	}
	for i := range m.vms {
		m.cursor = i
		if vm, ok := m.selectedVM(); ok && vm.Host == "home1" {
			t.Error("the network home row is selectable as a VM")
		}
	}
	if m.svc().NetworkHome == nil || !m.svc().NetworkHome("home1") || m.svc().NetworkHome("hv1") {
		t.Error("the service the TUI creates with does not know the network home")
	}
}

// The all-hosts refresh does not ask a network home: through newModel's own
// listing (the real app.ListAll over the resolver) the home is never opened
// and its row is the network-home row.
func TestTUIRefreshDoesNotAskNetworkHome(t *testing.T) {
	r := hosts.NewWith("ws", config.RoleWorkstation, []string{"hv1", "home1"}, func() (app.Manager, func(), error) {
		return &tuiFake{host: "ws"}, func() {}, nil
	}).WithNetworkHomes("home1").WithToken("PLACEHOLDER-test-token").WithRemote(func(host, _ string) (app.Manager, func(), error) {
		if host == "home1" {
			t.Error("the network home was opened")
			return nil, nil, errors.New("home1 unreachable")
		}
		return &tuiFake{host: host}, func() {}, nil
	})
	m := newModel(r, vmtool.Tunnel{}, 0, 0)
	start := time.Now()
	msg := m.refreshCmd()().(refreshMsg)
	if time.Since(start) > 2*time.Second {
		t.Errorf("refresh took %v", time.Since(start))
	}
	next, _ := m.Update(msg)
	out := next.(model).View()
	if !strings.Contains(out, "home1") || !strings.Contains(out, "network home: runs no VMs") || strings.Contains(out, "ERROR") {
		t.Errorf("view lacks the plain network home row:\n%s", out)
	}
}
