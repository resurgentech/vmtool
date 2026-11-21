package scripts_test

import (
	"os"
	"strings"
	"testing"
)

// N12: the docs say vmtool writes nothing of its own into libvirt; origin and
// comment are in the store only, in plain text; origin is recorded once at
// create and only with a store; the comment can be changed while the host
// sleeps and needs a store. And no doc still says they are kept in the VM's
// definition. Reads two text files only.

func TestVMNotesDocsKeywords(t *testing.T) {
	for _, f := range []struct {
		path string
		keys []string
	}{
		{"../README.md", []string{
			"A VM's notes in the store: origin and comment",
			"vmtool writes nothing of\nits own into a VM's libvirt definition",
			"lives in the store only, in plain text",
			"is recorded once, by `vmtool create`",
			"only with a store in use",
			"It\n  needs a store",
			"while the host sleeps",
		}},
		{"../spec/README.md", []string{
			"## A VM's notes in the store (API 2.6.0)",
			"vmtool writes nothing of its own into a VM's libvirt definition",
			"lives in the store only, in plain text",
			"written once",
			"only with a store in\n  use",
			"without asking the host (it may sleep)",
			"503 with\n  no store in use",
		}},
	} {
		b, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		for _, k := range f.keys {
			if !strings.Contains(text, k) {
				t.Errorf("%s lacks %q", f.path, k)
			}
		}
		for _, stale := range []string{"in the VM's libvirt definition and in", "domain's metadata", "kept with the VM", "## Origin and comment (API 2.6.0)"} {
			if strings.Contains(text, stale) {
				t.Errorf("%s still says %q", f.path, stale)
			}
		}
	}
}
