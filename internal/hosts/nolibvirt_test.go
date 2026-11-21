package hosts

import (
	"errors"
	"testing"

	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// G1: New(cfg)'s local connector is real libvirt. Under go test, resolving the
// local name returns pkg/vmtool's refusal instead of connecting. This test may
// only ever run with that guard in place.
func TestNewLocalRefusesLibvirtUnderTest(t *testing.T) {
	for _, role := range []string{config.RoleWorkstation, config.RoleHypervisor} {
		cfg := &config.Config{
			Name: "hv1", Role: role,
			Hosts: map[string]config.Host{"hv2": {Address: "192.0.2.12"}},
		}
		r := New(cfg)
		if r.IsRemote("hv1") {
			t.Fatalf("%s: hv1 is served remotely; the test must reach the local connector", role)
		}
		m, done, err := r.Resolve("hv1")
		if done != nil {
			done()
		}
		if m != nil {
			t.Fatalf("%s: Resolve(local) gave a Manager %T under go test", role, m)
		}
		if !errors.Is(err, vmtool.ErrLibvirtInTest) {
			t.Fatalf("%s: Resolve(local) = %v, want vmtool.ErrLibvirtInTest", role, err)
		}
	}
}
