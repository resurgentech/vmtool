package cli

import (
	"strings"
	"testing"
)

// The help of the inventory commands (tranche 12) states the rules a reader
// of the inventory needs: the store never judges a host, how long a login
// outlives a delisting, when it is corrected, and how origin, comment and an
// unreadable disk list read.
func TestInventoryHelpPinsKeyWords(t *testing.T) {
	root, _ := rootFor(t.TempDir(), nil, nil)
	for _, tc := range []struct {
		path []string
		want []string
	}{
		{[]string{"inventory"}, []string{
			"never decides whether a host is up", "sleeping host", "7 days", "empty report",
			"vmtool delete", "outside vmtool", "next report", "touch",
			"from this version on", "plain text", "ip_wait_seconds", "its own clock",
			"disks unreadable", "last known",
		}},
		{[]string{"comment"}, []string{"plain text", "store only", "never put a secret", "needs a store", "while the host sleeps"}},
		{[]string{"create"}, []string{"--comment", "plain text", "in the store", "once", "nothing of its own into the VM's libvirt definition",
			"no store in use no origin is recorded"}},
	} {
		c, _, err := root.Find(tc.path)
		if err != nil || c == nil || c == root {
			t.Fatalf("no command %v: %v", tc.path, err)
		}
		low := strings.ToLower(c.Long)
		for _, w := range tc.want {
			if !strings.Contains(low, strings.ToLower(w)) {
				t.Errorf("%s help lacks %q", c.CommandPath(), w)
			}
		}
	}
}
