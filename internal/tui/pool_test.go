package tui

import (
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// formFake answers the listings the create form fills its pickers from. Its
// pools are a shared hypervisor's: none is called "default".
type formFake struct{ app.Manager }

func (formFake) ListImagesByPool() (map[string][]string, error) {
	return map[string][]string{"tp": {"u.qcow2"}}, nil
}
func (formFake) ListNetworks() ([]string, error) { return []string{"default"}, nil }
func (formFake) ListBridges() ([]string, error)  { return nil, nil }
func (formFake) ListPools() ([]vmtool.PoolInfo, error) {
	return []vmtool.PoolInfo{{Name: "boot-scratch"}, {Name: "tp"}, {Name: "vt"}}, nil
}

// The create form's first Pool option is "(host default)", selected unless
// the user picks another, and sent as the empty pool so the host that owns
// libvirt chooses (default on a workstation, vmtool on a hypervisor).
func TestCreateFormPoolHostDefault(t *testing.T) {
	mm, _ := initialModel(formFake{}, "jetfire", t.TempDir(), "inv.yml").enterCreateMode()
	m := mm.(model)
	opts := m.formFields[fPool].options
	if len(opts) == 0 || opts[0] != hostDefaultPool {
		t.Fatalf("Pool options %q, want %q first", opts, hostDefaultPool)
	}
	if m.formValues[fPool] != hostDefaultPool {
		t.Fatalf("Pool preselected %q, want %q", m.formValues[fPool], hostDefaultPool)
	}
	m.formValues[fName] = "veritech04/web"
	in, ok, err := m.createInput()
	if err != nil || !ok {
		t.Fatalf("createInput: ok=%v err=%v", ok, err)
	}
	if in.Pool != "" || in.Host != "veritech04" || in.Name != "web" || in.Image != "u.qcow2" {
		t.Errorf("input %+v, want the empty pool on veritech04/web", in)
	}

	m.formValues[fPool] = "tp"
	if in, _, _ := m.createInput(); in.Pool != "tp" {
		t.Errorf("picked tp, input pool %q", in.Pool)
	}
}
