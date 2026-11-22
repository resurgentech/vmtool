package hosts

import (
	"fmt"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// V3: every wrapper keeps app.VNICreator visible, so a host's network number
// reaches the Manager underneath.
type vniFake struct {
	app.Manager
	calls []string
}

func (f *vniFake) CreateNetworkVNI(name, subnet, domain string, vni uint32) (*vmtool.NetworkInfo, error) {
	f.calls = append(f.calls, fmt.Sprintf("%s %s %s %d", name, subnet, domain, vni))
	return &vmtool.NetworkInfo{Name: name, Bridge: vmtool.BridgeName(vni)}, nil
}

// plainFake has no CreateNetworkVNI.
type plainFake struct {
	app.Manager
	calls []string
}

func (f *plainFake) CreateNetwork(name, subnet, domain string) (*vmtool.NetworkInfo, error) {
	f.calls = append(f.calls, "plain "+name)
	return &vmtool.NetworkInfo{Name: name, Bridge: "vmt-000001"}, nil
}

func TestWrappersForwardCreateNetworkVNI(t *testing.T) {
	for name, wrap := range map[string]func(app.Manager) app.Manager{
		"hypervisorPools": func(m app.Manager) app.Manager { return hypervisorPools{m} },
		"defaultPools":    func(m app.Manager) app.Manager { return withDefaultPools(m, "nvme") },
		"ThinPools":       func(m app.Manager) app.Manager { return ThinPools{Manager: m} },
	} {
		f := &vniFake{}
		w, ok := wrap(f).(app.VNICreator)
		if !ok {
			t.Errorf("%s hides app.VNICreator", name)
			continue
		}
		n, err := w.CreateNetworkVNI("net1", "10.77.1.0/24", "lab.example", 10001)
		if err != nil || n.Bridge != "vmt-002711" || len(f.calls) != 1 || f.calls[0] != "net1 10.77.1.0/24 lab.example 10001" {
			t.Errorf("%s: %+v %v, calls %v", name, n, err, f.calls)
		}
		// Over a Manager that cannot take a number: none given is the plain
		// create, one given is refused.
		p := &plainFake{}
		w = wrap(p).(app.VNICreator)
		if _, err := w.CreateNetworkVNI("net2", "", "", 0); err != nil || len(p.calls) != 1 {
			t.Errorf("%s: no number over a plain host: %v, %v", name, err, p.calls)
		}
		if _, err := w.CreateNetworkVNI("net2", "", "", 10001); err == nil {
			t.Errorf("%s: a number over a host that cannot take one was accepted", name)
		}
	}
}
