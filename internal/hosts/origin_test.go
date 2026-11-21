package hosts

import (
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// snapshotOf is a thin vmtool_vol row that is a snapshot of origin.
func snapshotOf(name, origin string) string {
	return strings.Replace(thinLV(name, 20*gib, "vmtool_vol"), `"origin":""`, `"origin":"`+origin+`"`, 1)
}

// On a thin pool the daemon knows what the app only guessed: the boot disk is
// a snapshot of a template, so the origin it records says thin, names the
// template and its LV, and carries the template's digest.
func TestThinDefineRecordsTemplateOrigin(t *testing.T) {
	created := time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)
	for _, c := range []struct {
		name, tplLV, tags         string
		wantName, wantLV, wantSHA string
	}{
		{"image template", tplLV, tplTagsA, "img1", tplLV, sha64a},
		{"saved template", savedLV, savedTags, "proj", savedLV, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHelperStub(t)
			h.report(t, reportOf(poolRow("25.00"), thinLV(c.tplLV, 10*gib, c.tags), snapshotOf("vm1", c.tplLV)))
			inner := &thinInner{}
			m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
			given := &vmtool.VMOrigin{Image: "img1", CloneKind: vmtool.CloneKindBlock, Pool: "thin-test", Created: created}
			cfg := vmtool.VMConfig{Name: "vm1", DiskPath: "/dev/vg-test/vm1", Pool: "thin-test", Origin: given}
			if err := m.Define(cfg); err != nil {
				t.Fatal(err)
			}
			want := &vmtool.VMOrigin{Image: "img1", ImageSHA256: c.wantSHA, Template: c.wantName, TemplateLV: c.wantLV,
				CloneKind: vmtool.CloneKindThin, Pool: "thin-test", Created: created}
			if got := inner.lastDefined().Origin; *got != *want {
				t.Errorf("origin %+v, want %+v", got, want)
			}
			if given.CloneKind != vmtool.CloneKindBlock || given.Template != "" {
				t.Error("Define changed the caller's origin")
			}
		})
	}
}

// No origin stays none; a disk that is not a thin volume is left as sent; a
// digest the create knew is not replaced.
func TestThinDefineOriginUntouched(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV(tplLV, 10*gib, tplTagsA), snapshotOf("vm1", tplLV), thinLV("foreign", gib, "")))
	inner := &thinInner{}
	m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))

	if err := m.Define(vmtool.VMConfig{Name: "vm1", DiskPath: "/dev/vg-test/vm1"}); err != nil {
		t.Fatal(err)
	}
	if inner.lastDefined().Origin != nil {
		t.Errorf("origin appeared from nothing: %+v", inner.lastDefined().Origin)
	}

	o := &vmtool.VMOrigin{Image: "img1", CloneKind: vmtool.CloneKindBlock}
	if err := m.Define(vmtool.VMConfig{Name: "web", DiskPath: "/dev/vg-test/foreign", Origin: o}); err != nil {
		t.Fatal(err)
	}
	if got := inner.lastDefined().Origin; *got != *o {
		t.Errorf("a non-vmtool volume changed the origin: %+v", got)
	}

	known := strings.Repeat("c", 64)
	o = &vmtool.VMOrigin{Image: "img1", ImageSHA256: known, CloneKind: vmtool.CloneKindBlock}
	if err := m.Define(vmtool.VMConfig{Name: "vm1", DiskPath: "/dev/vg-test/vm1", Origin: o}); err != nil {
		t.Fatal(err)
	}
	if got := inner.lastDefined().Origin; got.ImageSHA256 != known || got.CloneKind != vmtool.CloneKindThin {
		t.Errorf("origin %+v, want the create's digest kept and kind thin", got)
	}
}
