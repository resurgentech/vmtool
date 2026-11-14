package cli

import (
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/config"
)

// No --pool is the host's default pool, which the host resolves: every pool
// flag defaults to empty, and its help says so.
func TestPoolFlagsDefaultEmpty(t *testing.T) {
	root, _ := rootFor(t.TempDir(), nil, nil)
	for _, tc := range []struct {
		path []string
		flag string
	}{
		{[]string{"create"}, "pool"},
		{[]string{"create"}, "extra-disk-pool"},
		{[]string{"add-disk"}, "pool"},
		{[]string{"images", "delete"}, "pool"},
	} {
		c, _, err := root.Find(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		fl := c.Flags().Lookup(tc.flag)
		if fl == nil {
			t.Fatalf("%v --%s missing", tc.path, tc.flag)
		}
		if fl.DefValue != "" {
			t.Errorf("%v --%s defaults to %q, want empty (the host's default)", tc.path, tc.flag, fl.DefValue)
		}
		if strings.Contains(fl.Usage, "default: default") {
			t.Errorf("%v --%s help %q still names the default pool", tc.path, tc.flag, fl.Usage)
		}
	}
}

// `images delete` without --pool passes the empty pool to the host's
// Manager, local or remote.
func TestImagesDeleteNoPool(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	keep := map[string]bool{"vmtool images delete": true}
	for _, host := range []string{"jetfire", "veritech01"} {
		f := installFakeHosts(t)
		r := runGate(t, dir, keep, "images", "delete", "x.qcow2", "--host", host)
		if r.err != nil {
			t.Fatalf("%s: %v", host, r.err)
		}
		ops, _ := f.seen()
		if len(ops) != 1 || ops[0] != host+" DeleteImage x.qcow2 " {
			t.Errorf("%s: ops %q, want [%s DeleteImage x.qcow2 ]", host, ops, host)
		}
	}
}
