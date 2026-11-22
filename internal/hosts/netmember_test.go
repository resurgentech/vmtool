package hosts

import (
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/remote"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// V5: every wrapper gives up the Manager it wraps, so app.AsNetMember finds the
// network capabilities (members, links, the underlay) of the Manager underneath.
type memberFake struct {
	app.Manager
	links []string
}

func (f *memberFake) CreateNetworkMember(name string, vni uint32) (*vmtool.NetworkInfo, error) {
	return &vmtool.NetworkInfo{Name: name, Bridge: vmtool.BridgeName(vni)}, nil
}
func (f *memberFake) SetNetworkLinks(name, ip, dev string, peers []string) error {
	f.links = append(f.links, name+" "+ip+" "+dev)
	return nil
}
func (f *memberFake) RemoveNetworkLink(string) error { return nil }
func (f *memberFake) Underlay(string) (*vmtool.Underlay, error) {
	return &vmtool.Underlay{IP: "192.0.2.11", Dev: "eth-test"}, nil
}

// The two real Managers have the capability (a compile-time check: without it
// the daemon would say "redeploy" to every join).
var (
	_ app.NetMember = (*vmtool.Manager)(nil)
	_ app.NetMember = (*remote.Manager)(nil)
)

func TestWrappersKeepNetMemberReachable(t *testing.T) {
	for name, wrap := range map[string]func(app.Manager) app.Manager{
		"hypervisorPools": func(m app.Manager) app.Manager { return hypervisorPools{m} },
		"defaultPools":    func(m app.Manager) app.Manager { return withDefaultPools(m, "nvme") },
		"ThinPools":       func(m app.Manager) app.Manager { return ThinPools{Manager: m} },
		"two deep":        func(m app.Manager) app.Manager { return hypervisorPools{ThinPools{Manager: m}} },
	} {
		f := &memberFake{}
		nm, ok := app.AsNetMember(wrap(f))
		if !ok {
			t.Errorf("%s hides the network member capability", name)
			continue
		}
		if err := nm.SetNetworkLinks("net1", "192.0.2.11", "eth-test", nil); err != nil || len(f.links) != 1 {
			t.Errorf("%s: %v, %v", name, err, f.links)
		}
		// A Manager without it stays without it.
		if _, ok := app.AsNetMember(wrap(&plainFake{})); ok {
			t.Errorf("%s invents a network member capability", name)
		}
	}
}
