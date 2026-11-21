package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// The pane under the VM list shows the selected VM's origin and comment, and
// nothing for a VM that has neither.
func TestViewShowsSelectedVMOriginAndComment(t *testing.T) {
	m := initialModel(&tuiFake{host: "jetfire"}, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	m.vms = []vmRow{
		{Host: "jetfire", VMInfo: vmtool.VMInfo{Name: "vm1", State: vmtool.StateRunning, Comment: "placeholder comment",
			Origin: &vmtool.VMOrigin{Image: "img1", CloneKind: vmtool.CloneKindThin, Pool: "nvme-thin",
				Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)}}},
		{Host: "jetfire", VMInfo: vmtool.VMInfo{Name: "vm2", State: vmtool.StateRunning}},
	}
	m.cursor = 0
	out := m.View()
	for _, want := range []string{"placeholder comment", "img1", "thin", "nvme-thin"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	m.cursor = 1
	out = m.View()
	if strings.Contains(out, "placeholder comment") || strings.Contains(out, "origin:") {
		t.Errorf("a VM with neither shows them:\n%s", out)
	}
}
