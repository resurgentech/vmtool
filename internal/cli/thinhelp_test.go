package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The help of the thin-pool commands (tranche 11) says what an author must
// know before using them: what a thin create clones from, what it refuses,
// where a template lives, and where the requirements are written.
func TestThinHelpNamesTheThinPath(t *testing.T) {
	root, _ := rootFor(t.TempDir(), nil, nil)
	find := func(path ...string) *cobra.Command {
		t.Helper()
		c, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		if c == nil || c == root {
			t.Fatalf("no command %v", path)
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
	create := find("create")
	for _, tc := range []struct {
		what, text string
		want       []string
	}{
		{"create --pool usage", usage(create, "pool"), []string{"thin", "template"}},
		{"create --extra-disk-size usage", usage(create, "extra-disk-size"), []string{"thin", "add-disk"}},
		{"create --disk-cache usage", usage(create, "disk-cache"), []string{"thin"}},
		{"create Long", create.Long, []string{
			"thin pool", "template", "--noclone", "add-disk", "timings:", "What a template must be", "per host",
		}},
		{"templates Long", find("templates").Long, []string{
			"per host", "What a template must be", "save", "export", "images pull", "does not check",
		}},
		{"templates save Long", find("templates", "save").Long, []string{
			"What a template must be", "does not check", "shut off", "stop",
		}},
		{"templates export Long", find("templates", "export").Long, []string{"per host", "images pull"}},
		{"images pull Long", find("images", "pull").Long, []string{"templates export", "per host", "images push"}},
		{"pool list Long", find("pool", "list").Long, []string{"DATA%", "META%", "VIRTUAL", "80", "4x"}},
		{"migrate-disk Long", find("migrate-disk").Long, []string{"thin"}},
	} {
		for _, w := range tc.want {
			if !strings.Contains(tc.text, w) {
				t.Errorf("%s lacks %q: %q", tc.what, w, tc.text)
			}
		}
	}
}
