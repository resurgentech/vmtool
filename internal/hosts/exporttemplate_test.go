package hosts

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// The daemon side of `templates export`: LookupTemplate, ExportTemplateVolume
// and RecordTemplateSHA. The helper is the stub sudo of thinclone_test.go; no
// real lvm, qemu-img or sudo runs.

func exporter(t *testing.T, m ThinPools) app.TemplateVolumeExporter {
	t.Helper()
	var x app.TemplateVolumeExporter = m
	return x
}

func savedReport(tags string) string {
	return reportOf(poolRow("25.00"), thinLV(savedLV, 10*gib, tags), thinLV("vm1", 20*gib, "vmtool_vol"))
}

func TestLookupTemplateFindsExactlyOne(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, savedReport(savedTags))
	m := newThinFor(t, &thinInner{}, cache{})
	x := exporter(t, m)

	for _, name := range []string{"proj", savedLV} {
		got, err := x.LookupTemplate(name, "")
		if err != nil || got.LV != savedLV || got.Name != "proj" || got.Origin != vmtool.TemplateOriginSaved {
			t.Errorf("LookupTemplate(%q) = %+v, %v", name, got, err)
		}
	}
	// a VM's volume is never a template
	if _, err := x.LookupTemplate("vm1", ""); !vmtool.IsNotFound(err) {
		t.Errorf("LookupTemplate(vm1) = %v, want not found", err)
	}
	if _, err := x.LookupTemplate("proj", "other-pool"); !vmtool.IsNotFound(err) {
		t.Errorf("LookupTemplate in another pool = %v, want not found", err)
	}
	if n := len(h.writes(t)); n != 0 {
		t.Errorf("a lookup wrote: %q", h.writes(t))
	}

	// two templates of one name: a conflict listing both, nothing chosen
	h.report(t, reportOf(poolRow("25.00"), thinLV(savedLV, 10*gib, savedTags),
		thinLV("vmtpl-000000000002", 10*gib, savedTags)))
	_, err := x.LookupTemplate("proj", "")
	var ae *app.Error
	if !errors.As(err, &ae) || ae.Kind != app.KindConflict || !strings.Contains(err.Error(), "vmtpl-000000000002") {
		t.Errorf("two matches = %v, want a conflict naming both", err)
	}
}

// The export names a volume only if the report, read again under the lock,
// still lists it as a template: a VM's volume or a vanished LV is refused with
// no helper write.
func TestExportTemplateVolumeRefusesWhatIsNotATemplate(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, savedReport(savedTags))
	m := newThinFor(t, &thinInner{}, cache{})
	x := exporter(t, m)

	for name, info := range map[string]app.TemplateInfo{
		"a vm volume":  {Template: vmtool.Template{Name: "vm1", LV: "vm1", Pool: "thin-test", Origin: vmtool.TemplateOriginSaved}},
		"gone":         {Template: vmtool.Template{Name: "proj", LV: "vmtpl-0000000000ff", Pool: "thin-test", Origin: vmtool.TemplateOriginSaved}},
		"another name": {Template: vmtool.Template{Name: "other", LV: savedLV, Pool: "thin-test", Origin: vmtool.TemplateOriginSaved}},
	} {
		if _, err := x.ExportTemplateVolume(info, nil); err == nil {
			t.Errorf("%s: exported", name)
		}
	}
	if n := len(h.writes(t)); n != 0 {
		t.Errorf("helper writes: %q", h.writes(t))
	}
}

func TestRecordTemplateSHATagsTheTemplate(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, savedReport(savedTags))
	m := newThinFor(t, &thinInner{}, cache{})
	x := exporter(t, m)
	tpl, _ := x.LookupTemplate("proj", "")

	if err := x.RecordTemplateSHA(tpl, sha64a); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "tag", h.writes(t), []string{"tag vg-test " + savedLV + " +vmtool_sha256=" + sha64a})

	// a template that already carries another digest has it replaced, in one call
	h2 := newHelperStub(t)
	h2.report(t, savedReport(savedTags+",vmtool_sha256="+sha64b))
	tpl, _ = exporter(t, newThinFor(t, &thinInner{}, cache{})).LookupTemplate("proj", "")
	if tpl.SHA256 != sha64b {
		t.Fatalf("template sha %q", tpl.SHA256)
	}
	if err := exporter(t, newThinFor(t, &thinInner{}, cache{})).RecordTemplateSHA(tpl, sha64a); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "tag replace", h2.writes(t), []string{
		"tag vg-test " + savedLV + " -vmtool_sha256=" + sha64b + " +vmtool_sha256=" + sha64a})

	// not a digest: nothing is written
	h3 := newHelperStub(t)
	h3.report(t, savedReport(savedTags))
	if err := x.RecordTemplateSHA(tpl, "nothex"); err == nil {
		t.Error("a bad digest was recorded")
	}
	if n := len(h3.writes(t)); n != 0 {
		t.Errorf("writes: %q", h3.writes(t))
	}
}

// After an export the template carries the image's SHA-256 and the manifest
// lists the file: the next create of that image name on the host finds the
// template by (name, sha) and clones it. No import, no create: no rebuild.
func TestCreateAfterExportUsesTheTemplateWithoutRebuilding(t *testing.T) {
	dir := t.TempDir()
	b := make([]byte, 72)
	copy(b, []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint64(b[24:], uint64(10*gib))
	img := filepath.Join(dir, "proj")
	if err := os.WriteFile(img, b, 0o644); err != nil {
		t.Fatal(err)
	}
	man := filepath.Join(t.TempDir(), "image-manifest.yml")
	if err := os.WriteFile(man, []byte(fmt.Sprintf("proj:\n  sha256: %s\n  size: %d\n", sha64a, len(b))), 0o644); err != nil {
		t.Fatal(err)
	}
	c := cache{dir: dir, manifest: man, img: img}

	h := newHelperStub(t)
	h.report(t, savedReport(savedTags+",vmtool_sha256="+sha64a))
	m := newThinFor(t, &thinInner{}, c)
	path, err := m.CloneImageFrom("proj", img, "vm9", "thin-test", uint64(10*gib))
	if err != nil || path != "/dev/vg-test/vm9" {
		t.Fatalf("CloneImageFrom = %q, %v", path, err)
	}
	eqStrings(t, "helper argv", h.calls(t), []string{"report", "snapshot vg-test " + savedLV + " vm9 vmtool_vol"})
	for _, verb := range []string{"import", "create"} {
		if n := h.count(t, verb); n != 0 {
			t.Errorf("%d helper %s calls: the template was rebuilt", n, verb)
		}
	}

	// A saved template whose digest is not the cached file's is not used: the
	// cached image is built as before.
	h2 := newHelperStub(t)
	h2.report(t, savedReport(savedTags+",vmtool_sha256="+sha64b))
	m = newThinFor(t, &thinInner{}, c)
	_, _ = m.CloneImageFrom("proj", img, "vm9", "thin-test", uint64(10*gib))
	if h2.count(t, "create") != 1 || strings.Contains(strings.Join(h2.calls(t), "\n"), "snapshot vg-test "+savedLV) {
		t.Errorf("a saved template of another digest was used: %q", h2.calls(t))
	}
}

// The wrappers every daemon Manager sits in keep the interface visible.
func TestTemplateExportSurvivesTheWrappers(t *testing.T) {
	var _ app.TemplateVolumeExporter = hypervisorPools{}
	var _ app.TemplateVolumeExporter = defaultPools{}
}
