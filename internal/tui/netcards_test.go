package tui

import (
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// V2: the create form takes extra cards on vmtool networks, and the detail line
// shows the selected VM's cards.

func TestCreateFormNetworksField(t *testing.T) {
	mm, _ := initialModel(formFake{}, "jetfire", t.TempDir(), "inv.yml").enterCreateMode()
	m := mm.(model)
	f := m.formFields[fNetworks]
	if !strings.HasPrefix(f.label, "Networks") || f.kind != fieldText || f.def != "" {
		t.Fatalf("field = %+v, want a text field labelled Networks, empty by default", f)
	}
	m.formValues[fName] = "web"
	if in, ok, err := m.createInput(); err != nil || !ok || len(in.Networks) != 0 {
		t.Fatalf("default: ok=%v err=%v networks=%+v, want none", ok, err, in.Networks)
	}
	m.formValues[fNetworks] = "net1, net2=10.77.2.50"
	in, ok, err := m.createInput()
	want := []app.NetworkRequest{{Network: "net1"}, {Network: "net2", IP: "10.77.2.50"}}
	if err != nil || !ok || len(in.Networks) != 2 || in.Networks[0] != want[0] || in.Networks[1] != want[1] {
		t.Fatalf("ok=%v err=%v networks=%+v, want %+v", ok, err, in.Networks, want)
	}
	// a bad entry is an error shown to the user, not a silent create
	m.formValues[fNetworks] = "net1, =10.77.1.5"
	if _, _, err := m.createInput(); err == nil {
		t.Error("a nameless entry was accepted")
	}
}

func TestVMDetailShowsCards(t *testing.T) {
	m := initialModel(formFake{}, "jetfire", t.TempDir(), "inv.yml")
	row := vmRow{Host: "jetfire", VMInfo: vmtool.VMInfo{Name: "web", Networks: []vmtool.NetAttachment{
		{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.1.100"},
		{Network: "net2", MAC: "52:54:00:00:00:02"},
	}}}
	got := m.vmDetail(row)
	if !strings.Contains(got, "networks: net1 10.77.1.100, net2 -") {
		t.Fatalf("detail = %q", got)
	}
	if d := m.vmDetail(vmRow{Host: "jetfire", VMInfo: vmtool.VMInfo{Name: "web"}}); d != "" {
		t.Fatalf("a VM with no cards and no notes has detail %q", d)
	}
}
