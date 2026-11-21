package cli

import (
	"strings"
	"testing"
)

// The help of the export commands (tranche 13) says where an export lives,
// how long, what it costs in room, and how the checksum is checked.
func TestExportHelpNamesTheArray(t *testing.T) {
	root, _ := rootFor(t.TempDir(), nil, nil)
	for _, tc := range []struct {
		path []string
		want []string
	}{
		{[]string{"templates", "export"}, []string{
			"NVMe array", "24 hours", "vmexp-", "virsh vol-list", "not in the image cache",
			"images delete", "free outside the thin pool", "10 %", "no fallback", "templates prune",
		}},
		{[]string{"images", "pull"}, []string{
			"NVMe array", "24 hours", "export", "after the last byte", "nothing is kept",
		}},
		{[]string{"templates", "list"}, []string{"EXPORT", "expires", "sha256"}},
		{[]string{"templates", "prune"}, []string{"export", "24 hours", "vmexp-"}},
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
