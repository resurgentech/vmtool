package tui

import "testing"

// The create form offers Disk cache, "default" first (the app resolves it),
// and passes a named mode on.
func TestCreateFormDiskCache(t *testing.T) {
	mm, _ := initialModel(formFake{}, "jetfire", t.TempDir(), "inv.yml").enterCreateMode()
	m := mm.(model)
	opts := m.formFields[fDiskCache].options
	want := []string{"default", "none", "writeback", "writethrough", "unsafe"}
	if len(opts) != len(want) {
		t.Fatalf("options %q, want %q", opts, want)
	}
	for i := range want {
		if opts[i] != want[i] {
			t.Fatalf("options %q, want %q", opts, want)
		}
	}
	m.formValues[fName] = "web"
	in, ok, err := m.createInput()
	if err != nil || !ok || in.DiskCache != "" {
		t.Fatalf("default: ok=%v err=%v cache=%q, want empty", ok, err, in.DiskCache)
	}
	m.formValues[fDiskCache] = "none"
	if in, _, _ := m.createInput(); in.DiskCache != "none" {
		t.Errorf("picked none, input cache %q", in.DiskCache)
	}
}
