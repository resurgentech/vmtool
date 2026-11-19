package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

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

// Disk help names both pool kinds (tranche 7): a dir pool holds qcow2 files,
// a logical pool LVs; an empty --pool takes the host's default_pool; a clone
// in a logical pool gets cache none; an LV cannot be grown or migrated.
func TestDiskHelpNamesPoolKinds(t *testing.T) {
	root, _ := rootFor(t.TempDir(), nil, nil)
	find := func(path ...string) *cobra.Command {
		t.Helper()
		c, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	usage := func(c *cobra.Command, flag string) string {
		t.Helper()
		fl := c.Flags().Lookup(flag)
		if fl == nil {
			t.Fatalf("%s --%s missing", c.CommandPath(), flag)
		}
		return fl.Usage
	}
	create, addDisk := find("create"), find("add-disk")
	for _, tc := range []struct {
		what, text string
		want       []string
	}{
		{"create --pool usage", usage(create, "pool"), []string{"default_pool", "logical"}},
		{"add-disk --pool usage", usage(addDisk, "pool"), []string{"default_pool", "logical"}},
		{"create --extra-disk-size usage", usage(create, "extra-disk-size"), []string{"qcow2", "logical"}},
		{"add-disk Long", addDisk.Long, []string{"qcow2", "logical", "thin"}},
		{"create --disk-cache usage", usage(create, "disk-cache"), []string{"logical"}},
		{"create Long", create.Long, []string{"logical"}},
		{"resize-disk Long", find("resize-disk").Long, []string{"logical", "thin", "grows"}},
		{"migrate-disk Long", find("migrate-disk").Long, []string{"logical"}},
	} {
		for _, w := range tc.want {
			if !strings.Contains(tc.text, w) {
				t.Errorf("%s lacks %q: %q", tc.what, w, tc.text)
			}
		}
	}
	if strings.HasPrefix(addDisk.Long, "Create an empty qcow2 volume") {
		t.Errorf("add-disk Long still starts %q", "Create an empty qcow2 volume")
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
