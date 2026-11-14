package tui

import (
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Rows name VMs host/vm; machines keep their bare names.
func TestRowsShowHostSlashVM(t *testing.T) {
	m := initialModel(nil, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	m.vms = []vmtool.VMInfo{{Name: "web", State: vmtool.StateRunning, IP: "10.0.0.5"}}
	out := m.View()
	if !strings.Contains(out, "jetfire/web") {
		t.Fatalf("view has no jetfire/web:\n%s", out)
	}
	if ref := m.vmRef(m.vms[0]); ref.Host != "jetfire" || ref.Name != "web" {
		t.Fatalf("vmRef = %+v", ref)
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
