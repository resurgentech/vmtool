package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// V1: N opens a read-only list of the host's networks with vmtool's marked.

type netFake struct{ app.Manager }

func (netFake) ListNetworks() ([]string, error) { return []string{"default", "net1", "net2"}, nil }
func (netFake) Network(name string) (*vmtool.NetworkInfo, error) {
	switch name {
	case "net1":
		return &vmtool.NetworkInfo{Name: name, Bridge: "vmt-000001", Vmtool: true, Forward: "nat", Gateway: "10.77.1.1"}, nil
	case "net2": // the member form of a network that spans hosts
		return &vmtool.NetworkInfo{Name: name, Bridge: "vmt-002711", Vmtool: true, VNI: 10001}, nil
	}
	return &vmtool.NetworkInfo{Name: name, Bridge: "virbr0"}, nil
}

func TestNetworkListMarksVmtoolNetworks(t *testing.T) {
	m := initialModel(netFake{}, "hv1", "ansible/playbooks", "ansible/inventory.yml")
	next, cmd := m.Update(key("N"))
	m = next.(model)
	if m.mode != modeNetworks || cmd == nil {
		t.Fatalf("N: mode %v, cmd %v", m.mode, cmd)
	}
	if out := m.View(); !strings.Contains(out, "Reading the host") {
		t.Errorf("before the answer:\n%s", out)
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	out := m.View()
	for _, want := range []string{"Networks on hv1", "  default\n", "  net1 (vmtool)\n", "  net2 (vmtool, member)\n", "esc=back"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "default (vmtool)") {
		t.Errorf("default marked as vmtool's:\n%s", out)
	}
	next, _ = m.Update(key("esc"))
	if next.(model).mode != modeList {
		t.Errorf("esc: mode %v", next.(model).mode)
	}
}

type netDownFake struct{ app.Manager }

func (netDownFake) ListNetworks() ([]string, error) { return nil, fmt.Errorf("host unreachable") }

func TestNetworkListShowsError(t *testing.T) {
	m := initialModel(netDownFake{}, "hv1", "ansible/playbooks", "ansible/inventory.yml")
	next, cmd := m.Update(key("N"))
	next, _ = next.(model).Update(cmd())
	if out := next.(model).View(); !strings.Contains(out, "host unreachable") {
		t.Errorf("error not shown:\n%s", out)
	}
}
