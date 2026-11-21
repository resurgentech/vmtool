package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/vmnotes"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// The pane under the VM list shows the selected VM's origin and comment, read
// from the store's notes (N12), and nothing for a VM that has neither or when
// the store was not read.
func TestViewShowsSelectedVMNotesFromStore(t *testing.T) {
	m := initialModel(&tuiFake{host: "jetfire"}, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	m.vms = []vmRow{
		{Host: "jetfire", VMInfo: vmtool.VMInfo{Name: "vm1", State: vmtool.StateRunning, UUID: "00000000-0000-4000-8000-000000000001"}},
		{Host: "jetfire", VMInfo: vmtool.VMInfo{Name: "vm2", State: vmtool.StateRunning}},
	}
	m.notes = map[app.VMRef]app.VMNotes{
		{Host: "jetfire", Name: "vm1"}: {UUID: "00000000-0000-4000-8000-000000000001", Comment: "placeholder comment",
			Origin: &vmnotes.Origin{Image: "img1", CloneKind: vmnotes.CloneKindThin, Pool: "nvme-thin",
				Created: time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)}},
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
	// Notes of another VM that had the name (another UUID) are not this VM's.
	m.cursor = 0
	m.vms[0].UUID = "00000000-0000-4000-8000-000000000009"
	if out := m.View(); strings.Contains(out, "placeholder comment") {
		t.Errorf("another UUID's notes shown:\n%s", out)
	}
	// No store read: nothing.
	m.notes = nil
	if out := m.View(); strings.Contains(out, "origin:") {
		t.Errorf("notes without the store:\n%s", out)
	}
}
