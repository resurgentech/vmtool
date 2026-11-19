package tui

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// daemonFake is a daemon's Manager: it lists VMs and answers the create
// form's pickers; anything else panics.
type daemonFake struct{ formFake }

func (daemonFake) List() ([]vmtool.VMInfo, error) { return []vmtool.VMInfo{{Name: "web"}}, nil }

func ownResolver(opened *atomic.Int32, listed bool) *hosts.Resolver {
	configured := []string{"hv1"}
	if listed {
		configured = append(configured, "ws")
	}
	return hosts.NewWith("ws", config.RoleWorkstation, configured, func() (app.Manager, func(), error) {
		opened.Add(1)
		return nil, nil, errors.New("libvirt: no socket")
	}).WithToken("PLACEHOLDER-not-a-secret-1").
		WithRemote(func(string, string) (app.Manager, func(), error) { return daemonFake{}, func() {}, nil })
}

// With the own name listed the TUI starts, lists, fills the create form and
// looks for a base image without opening libvirt: the daemon answers.
func TestOwnListedTUIOpensNoLibvirt(t *testing.T) {
	var opened atomic.Int32
	m := newModel(ownResolver(&opened, true), vmtool.Tunnel{}, 0, 0)

	rows := m.fetchRows()
	if len(rows) != 2 || rows[0].Host != "ws" || rows[0].Err != nil || rows[0].Name != "web" {
		t.Fatalf("rows %+v", rows)
	}
	if _, err := m.svc().LocalImages("u.qcow2"); !vmtool.IsNotFound(err) {
		t.Fatalf("LocalImages = %v, want not found", err)
	}
	mm, _ := m.enterCreateMode()
	if opts := mm.(model).formFields[fImage].options; len(opts) != 1 || opts[0] != "u.qcow2" {
		t.Fatalf("image options %q, want the daemon's", opts)
	}
	if n := opened.Load(); n != 0 {
		t.Fatalf("libvirt opened %d times", n)
	}
}

// With neither a daemon nor libvirt the TUI still starts: nothing is opened
// until an action asks, the own row says why, and the other hosts list.
func TestNeitherTUIStartsWithoutLibvirt(t *testing.T) {
	var opened atomic.Int32
	m := newModel(ownResolver(&opened, false), vmtool.Tunnel{}, 0, 0)
	if n := opened.Load(); n != 0 {
		t.Fatalf("libvirt opened %d times at start", n)
	}
	rows := m.listAll(context.Background())
	if len(rows) != 2 || rows[0].Err == nil || rows[1].Err != nil {
		t.Fatalf("rows %+v", rows)
	}
	if _, err := m.svc().LocalImages("u.qcow2"); !vmtool.IsNotFound(err) {
		t.Fatalf("LocalImages = %v, want not found", err)
	}
}
