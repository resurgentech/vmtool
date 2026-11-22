package hosts

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// saveInner is the fake libvirt under a save: one domain vm1 in the given
// state whose boot disk and extra targets are dom. Start records, so a test
// sees that the source VM still starts after a save.
type saveInner struct {
	*diskInner
	state   vmtool.VMState
	images  map[string]bool // cached image names ImagePath finds
	started []string
}

func (f *saveInner) Info(name string) (*vmtool.VMInfo, error) {
	f.rec("Info %s", name)
	return &vmtool.VMInfo{Name: name, State: f.state}, nil
}
func (f *saveInner) Start(name string) error {
	f.rec("Start %s", name)
	f.started = append(f.started, name)
	return nil
}
func (f *saveInner) ImagePath(name string) (string, error) {
	f.rec("ImagePath %s", name)
	if f.images[name] {
		return "/var/lib/vmtool/images/" + name, nil
	}
	return "", fmt.Errorf("image %q not found in any storage pool: %w", name, vmtool.ErrNotFound)
}

// saveFixture: vm1 (20 GiB, vmtool_vol) and vm1-2 in the pool at 25 %, plus
// the given extra report rows, a shut-off vm1 whose boot disk is vm1.
func saveFixture(t *testing.T, extra ...string) (*helperStub, *saveInner, ThinPools) {
	t.Helper()
	h := newHelperStub(t)
	rows := append([]string{poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_vol"), thinLV("vm1-2", 10*gib, "vmtool_vol")}, extra...)
	h.report(t, reportOf(rows...))
	dom := vmtool.DomainDisks{BootPath: "/dev/vg-test/vm1", BootKind: vmtool.DiskBackingBlock, Used: []string{"vda"}}
	inner := &saveInner{diskInner: &diskInner{thinInner: &thinInner{}, dom: dom}, state: vmtool.StateShutoff}
	return h, inner, newThinFor(t, inner, cache{})
}

var newTplLV = regexp.MustCompile(`^vmtpl-[0-9a-f]{12}$`)

// noWriteNamesSource fails when any helper write verb other than the one
// snapshot's origin argument names the source VM's volume (or the VM's extra
// disk): a save reads the source and writes nothing to it.
func noWriteNamesSource(t *testing.T, h *helperStub, srcs ...string) {
	t.Helper()
	for _, c := range h.writes(t) {
		f := strings.Fields(c)
		for i, a := range f[1:] {
			for _, s := range srcs {
				if a != s {
					continue
				}
				// snapshot VG SRC NAME TAG...: SRC is f[2]; i is the index in f[1:]
				if f[0] == "snapshot" && i == 1 {
					continue
				}
				t.Errorf("a write verb names the source volume %s: %q", s, c)
			}
		}
	}
}

func TestSaveTemplateSnapshotsTheBootDiskAndNothingElse(t *testing.T) {
	h, inner, m := saveFixture(t)
	got, err := m.SaveTemplate("vm1", "proj2", false)
	if err != nil {
		t.Fatal(err)
	}
	w := h.writes(t)
	if len(w) != 1 {
		t.Fatalf("write verbs %q, want exactly one snapshot", w)
	}
	f := strings.Fields(w[0])
	if len(f) != 7 || f[0] != "snapshot" || f[1] != "vg-test" || f[2] != "vm1" || !newTplLV.MatchString(f[3]) ||
		strings.Join(f[4:], ",") != "vmtool_template,vmtool_name=proj2,vmtool_origin=saved" {
		t.Errorf("snapshot argv %q", w[0])
	}
	if got.LV != f[3] || got.Name != "proj2" || got.Origin != vmtool.TemplateOriginSaved || got.Pool != "thin-test" || got.SizeBytes != 20*gib {
		t.Errorf("answer %+v", got)
	}
	if len(got.ExtraDisks) != 0 {
		t.Errorf("extra disks %v", got.ExtraDisks)
	}
	noWriteNamesSource(t, h, "vm1", "vm1-2")
	// libvirt is only read: no define, delete, resize, attach
	for _, e := range inner.events {
		if !strings.HasPrefix(e, "Info ") && !strings.HasPrefix(e, "DomainDisks ") && !strings.HasPrefix(e, "ImagePath ") {
			t.Errorf("libvirt call %q during a save", e)
		}
	}
	// the source VM still starts; nothing about it changed
	if err := m.Start("vm1"); err != nil || len(inner.started) != 1 {
		t.Errorf("Start after save: %v %v", err, inner.started)
	}
}

func TestSaveTemplateWarnsOfExtraDisks(t *testing.T) {
	h, inner, m := saveFixture(t)
	inner.dom.Used = []string{"vda", "vdb", "vdc"}
	got, err := m.SaveTemplate("vm1", "proj2", false)
	if err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "extra disks", got.ExtraDisks, []string{"vdb", "vdc"})
	if n := len(h.writes(t)); n != 1 {
		t.Errorf("%d writes, want one snapshot (the extra disks are not snapshotted)", n)
	}
	noWriteNamesSource(t, h, "vm1", "vm1-2")
}

func TestSaveTemplateRefusesAVMThatIsNotShutOff(t *testing.T) {
	for _, st := range []vmtool.VMState{vmtool.StateRunning, vmtool.StatePaused, vmtool.StateCrashed, vmtool.StateUnknown, vmtool.StateUndefined} {
		h, inner, m := saveFixture(t)
		inner.state = st
		_, err := m.SaveTemplate("vm1", "proj2", false)
		var ae *app.Error
		if !app.AsError(err, &ae) || ae.Kind != app.KindConflict || !strings.Contains(err.Error(), "shut it down first") || !strings.Contains(err.Error(), "vm1") {
			t.Errorf("%s: %v", st, err)
		}
		if w := h.writes(t); len(w) != 0 {
			t.Errorf("%s: writes %q", st, w)
		}
	}
}

func TestSaveTemplateRefusesADiskThatIsNotOnAThinPool(t *testing.T) {
	cases := map[string]vmtool.DomainDisks{
		"file disk":        {BootPath: "/var/lib/vmtool/images/vm1.qcow2", BootKind: vmtool.DiskBackingFile, Used: []string{"vda"}},
		"thick block":      {BootPath: "/dev/vg-other/vm1", BootKind: vmtool.DiskBackingBlock, Used: []string{"vda"}},
		"a template":       {BootPath: "/dev/vg-test/" + savedLV, BootKind: vmtool.DiskBackingBlock, Used: []string{"vda"}},
		"an untagged thin": {BootPath: "/dev/vg-test/foreign", BootKind: vmtool.DiskBackingBlock, Used: []string{"vda"}},
	}
	for name, dom := range cases {
		h, inner, m := saveFixture(t, thinLV(savedLV, 5*gib, savedTags), thinLV("foreign", gib, ""))
		inner.dom = dom
		_, err := m.SaveTemplate("vm1", "proj2", false)
		var ae *app.Error
		if !app.AsError(err, &ae) || ae.Kind != app.KindInvalid || !strings.Contains(err.Error(), "templates are saved from VMs on a thin pool") {
			t.Errorf("%s: %v", name, err)
		}
		if w := h.writes(t); len(w) != 0 {
			t.Errorf("%s: writes %q", name, w)
		}
	}
	// an inner Manager that cannot read disks is not thin either
	h := newHelperStub(t)
	h.report(t, warmReport())
	m := newThinFor(t, &thinInner{}, cache{})
	if _, err := m.SaveTemplate("vm1", "proj2", false); err == nil || !strings.Contains(err.Error(), "thin pool") {
		t.Errorf("no domain reader: %v", err)
	}
}

func TestSaveTemplateRefusesABadName(t *testing.T) {
	for _, name := range []string{"", "../x", "a b", "/dev/vg-test/vm1", "a/b", ".hidden", strings.Repeat("a", 200)} {
		h, _, m := saveFixture(t)
		_, err := m.SaveTemplate("vm1", name, false)
		var ae *app.Error
		if !app.AsError(err, &ae) || ae.Kind != app.KindInvalid {
			t.Errorf("%q: %v", name, err)
		}
		if w := h.writes(t); len(w) != 0 {
			t.Errorf("%q: writes %q", name, w)
		}
	}
}

func TestSaveTemplateRefusesANameInUse(t *testing.T) {
	extra := []string{
		thinLV(savedLV, 5*gib, savedTags), // saved "proj"
		thinLV(tplLV, 10*gib, tplTagsA),   // image template "img1"
		thinLV("vmtpl-000000000002", gib, savedTagsOf("by-lv")),
	}
	for _, tc := range []struct {
		name    string
		replace bool
		cached  bool
		want    string
	}{
		{"proj", false, false, "--replace"},
		{"img1", false, false, "image template"},
		{"img1", true, false, "image template"}, // replace never replaces an image template
		{"cachedimg", false, true, "cached image"},
		{"cachedimg", true, true, "cached image"}, // nor a cached image
		{savedLV, false, false, "in use"},         // an LV name, which delete also matches
	} {
		h, inner, m := saveFixture(t, extra...)
		inner.images = map[string]bool{"cachedimg": true}
		_, err := m.SaveTemplate("vm1", tc.name, tc.replace)
		var ae *app.Error
		if !app.AsError(err, &ae) || ae.Kind != app.KindConflict || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v", tc, err)
		}
		if w := h.writes(t); len(w) != 0 {
			t.Errorf("%+v: writes %q", tc, w)
		}
	}
}

func savedTagsOf(name string) string {
	return "vmtool_template,vmtool_name=" + name + ",vmtool_origin=saved"
}

// --replace makes the new template first, then removes exactly the old saved
// template, by its LV.
func TestSaveTemplateReplaceMakesTheNewOneThenRemovesTheOldSaved(t *testing.T) {
	h, _, m := saveFixture(t, thinLV(savedLV, 5*gib, savedTags), thinLV(tplLV, 10*gib, tplTagsA))
	got, err := m.SaveTemplate("vm1", "proj", true)
	if err != nil {
		t.Fatal(err)
	}
	w := h.writes(t)
	if len(w) != 2 || !strings.HasPrefix(w[0], "snapshot vg-test vm1 vmtpl-") || w[1] != "remove vg-test "+savedLV {
		t.Fatalf("write verbs %q", w)
	}
	if got.LV == savedLV || !newTplLV.MatchString(got.LV) {
		t.Errorf("new LV %q", got.LV)
	}
	noWriteNamesSource(t, h, "vm1", "vm1-2", tplLV)
	// replace with nothing to replace is a plain save
	h2, _, m2 := saveFixture(t)
	if _, err := m2.SaveTemplate("vm1", "fresh", true); err != nil {
		t.Fatal(err)
	}
	if w := h2.writes(t); len(w) != 1 || !strings.HasPrefix(w[0], "snapshot ") {
		t.Errorf("replace of nothing: %q", w)
	}
}

// A failed snapshot leaves the old saved template alone: replace never
// leaves a name with no template.
func TestSaveTemplateReplaceKeepsTheOldWhenTheSnapshotFails(t *testing.T) {
	h, _, m := saveFixture(t, thinLV(savedLV, 5*gib, savedTags))
	h.write(t, "fail_snapshot", "lvcreate failed")
	if _, err := m.SaveTemplate("vm1", "proj", true); err == nil {
		t.Fatal("no error")
	}
	for _, c := range h.writes(t) {
		if strings.HasPrefix(c, "remove") {
			t.Errorf("a removal after a failed snapshot: %q", c)
		}
	}
}

// The old template is removed only if, read again, it is still a saved
// template of that name; one that changed meanwhile is left.
func TestSaveTemplateReplaceReReadsBeforeRemoving(t *testing.T) {
	h, _, m := saveFixture(t, thinLV(savedLV, 5*gib, savedTags))
	h.write(t, "after.json", reportOf(poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_vol"), thinLV(savedLV, 5*gib, "")))
	h.write(t, "on_snapshot", `cp "$ST/after.json" "$ST/report.json"`)
	got, err := m.SaveTemplate("vm1", "proj", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range h.writes(t) {
		if strings.HasPrefix(c, "remove") {
			t.Errorf("removed %q although it is no longer a saved template", c)
		}
	}
	if !newTplLV.MatchString(got.LV) {
		t.Errorf("answer %+v", got)
	}
}

// A removal that fails after the new template exists is reported, and the
// error names the new template.
func TestSaveTemplateReplaceRemovalFailureNamesTheNewTemplate(t *testing.T) {
	h, _, m := saveFixture(t, thinLV(savedLV, 5*gib, savedTags))
	h.write(t, "fail_remove", "lvremove failed")
	_, err := m.SaveTemplate("vm1", "proj", true)
	if err == nil || !strings.Contains(err.Error(), savedLV) || !strings.Contains(err.Error(), "vmtpl-") {
		t.Errorf("err = %v", err)
	}
}

func TestSaveTemplateNoRoomWritesNothing(t *testing.T) {
	h, _, m := saveFixture(t)
	h.report(t, reportOf(poolRow("85.00"), thinLV("vm1", 20*gib, "vmtool_vol")))
	_, err := m.SaveTemplate("vm1", "proj2", false)
	var ae *app.Error
	if !errors.Is(err, vmtool.ErrThinNoRoom) || !app.AsError(err, &ae) || ae.Kind != app.KindConflict {
		t.Fatalf("err = %v", err)
	}
	if w := h.writes(t); len(w) != 0 {
		t.Errorf("writes %q", w)
	}
}

// The wrappers forward a save, and the host's default pool never applies to it.
func TestSaveTemplateThroughTheWrappers(t *testing.T) {
	_, _, m := saveFixture(t)
	for name, tm := range map[string]app.TemplateManager{
		"hypervisorPools": hypervisorPools{Manager: m},
		"defaultPools":    defaultPools{Manager: m},
	} {
		sm, ok := tm.(app.TemplateSaver)
		if !ok {
			t.Fatalf("%s is not an app.TemplateSaver", name)
		}
		if got, err := sm.SaveTemplate("vm1", "w-"+name, false); err != nil || got.Name != "w-"+name {
			t.Errorf("%s: %v %+v", name, err, got)
		}
	}
}
