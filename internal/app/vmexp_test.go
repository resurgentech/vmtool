package app

import (
	"context"
	"strings"
	"testing"
)

// A VM named like an export volume (vmexp-...) is refused on every pool,
// before any call to the host: the prefix belongs to export volumes in the
// array's volume group.
func TestCreateVMRefusesTheExportPrefix(t *testing.T) {
	for _, pool := range []string{"", "default", "thin-test"} {
		t.Chdir(t.TempDir())
		sshStub(t)
		m := &nwVMs{}
		_, err := (&Service{}).WithTunnel(appTunnel()).CreateVM(context.Background(), m,
			CreateVMInput{Host: "jetfire", Name: "vmexp-000000000001", Image: "u.qcow2", Pool: pool})
		ae := asAppErr(t, err)
		if ae.Kind != KindInvalid || !strings.Contains(err.Error(), "vmexp-") {
			t.Errorf("pool %q: err = %v, want an invalid naming the vmexp- prefix", pool, err)
		}
		if len(m.calls) != 0 {
			t.Errorf("pool %q: host calls made: %v", pool, m.calls)
		}
	}
}
