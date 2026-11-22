package hosts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

const (
	staleLV   = "vmtpl-bbbbbbbbbbbb" // img1 at an older SHA-256
	oldLV     = "vmtpl-cccccccccccc" // an image no longer in the manifest
	sha64c    = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	tplTagsB  = "vmtool_template,vmtool_name=img1,vmtool_sha256=" + sha64b + ",vmtool_origin=image"
	tplTagsC  = "vmtool_template,vmtool_name=old,vmtool_sha256=" + sha64c + ",vmtool_origin=image"
	cloneTags = "vmtool_vol"
)

// cloneLV is a vmtool_vol thin LV that is a snapshot of origin.
func cloneLV(name, origin string) string {
	return fmt.Sprintf(`{"lv_name":%q,"vg_name":"vg-test","lv_attr":"Vwi-aotz--","lv_size":"%d","pool_lv":"thin-test","origin":%q,"data_percent":"","metadata_percent":"","lv_tags":%q,"lv_time":""}`,
		name, 10*gib, origin, cloneTags)
}

// mixedReport: the current image template with two clones, a stale image
// template of img1, a stale one of an image the manifest lacks, a saved
// template, a building volume and a foreign (untagged) LV.
func mixedReport() string {
	return reportOf(poolRow("25.00"),
		thinLV(tplLV, 10*gib, tplTagsA),
		thinLV(staleLV, 10*gib, tplTagsB),
		thinLV(oldLV, 10*gib, tplTagsC),
		thinLV(savedLV, 5*gib, savedTags),
		cloneLV("vm1", tplLV),
		cloneLV("vm1-2", tplLV),
		thinLV(bldLV, gib, "vmtool_building"),
		thinLV("foreign", gib, ""),
	)
}

func newTemplateHost(t *testing.T, rep string) (ThinPools, *helperStub, *thinInner) {
	t.Helper()
	h := newHelperStub(t)
	h.report(t, rep)
	c := newCache(t, uint64(10*gib), sha64a)
	inner := &thinInner{}
	return newThinFor(t, inner, c), h, inner
}

func byLV(ts []app.TemplateInfo) map[string]app.TemplateInfo {
	out := map[string]app.TemplateInfo{}
	for _, tp := range ts {
		out[tp.LV] = tp
	}
	return out
}

func TestListTemplatesMarksStaleAndCountsClones(t *testing.T) {
	m, h, _ := newTemplateHost(t, mixedReport())
	got, err := m.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("%d templates, want 4 (building and foreign volumes are not templates): %+v", len(got), got)
	}
	by := byLV(got)
	cur := by[tplLV]
	if cur.Stale || cur.Clones != 2 || cur.Name != "img1" || cur.Image != "img1" || cur.SHA256 != sha64a ||
		cur.Origin != vmtool.TemplateOriginImage || cur.Pool != "thin-test" || cur.SizeBytes != 10*gib {
		t.Errorf("current: %+v", cur)
	}
	if !by[staleLV].Stale || !by[oldLV].Stale {
		t.Errorf("stale marks: %+v %+v", by[staleLV], by[oldLV])
	}
	if by[savedLV].Stale || by[savedLV].Origin != vmtool.TemplateOriginSaved {
		t.Errorf("a saved template is never stale: %+v", by[savedLV])
	}
	eqStrings(t, "helper argv (list reads only)", h.calls(t), []string{"report"})
}

// delete removes exactly the template LV; the clones are never named.
func TestDeleteTemplateWithClonesTouchesOnlyTheTemplate(t *testing.T) {
	m, h, _ := newTemplateHost(t, mixedReport())
	got, err := m.DeleteTemplate(tplLV, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.LV != tplLV || got.Clones != 2 {
		t.Errorf("answer %+v", got)
	}
	w := h.writes(t)
	eqStrings(t, "write verbs", w, []string{"remove vg-test " + tplLV})
	for _, c := range h.calls(t) {
		if strings.Contains(c, "vm1") {
			t.Errorf("a clone was named to the helper: %q", c)
		}
	}
}

// By the human name too; the name of the current template is unique here
// once the stale twin is addressed by its LV.
func TestDeleteTemplateByNameAndAmbiguity(t *testing.T) {
	m, h, _ := newTemplateHost(t, mixedReport())
	// "img1" is the current and a stale template: refused, nothing removed,
	// the answer lists the LV names.
	_, err := m.DeleteTemplate("img1", "")
	var ae *app.Error
	if !app.AsError(err, &ae) || ae.Kind != app.KindConflict || !strings.Contains(err.Error(), tplLV) || !strings.Contains(err.Error(), staleLV) {
		t.Fatalf("ambiguous: %v", err)
	}
	if len(h.writes(t)) != 0 {
		t.Errorf("writes %q", h.writes(t))
	}
	// the saved template by its name
	if _, err := m.DeleteTemplate("proj", "thin-test"); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "write verbs", h.writes(t), []string{"remove vg-test " + savedLV})
	// a pool that is not the template's
	_, err = m.DeleteTemplate("old", "other-pool")
	if !app.AsError(err, &ae) || ae.Kind != app.KindNotFound {
		t.Errorf("other pool: %v", err)
	}
}

// Only a volume the report lists as a template is ever removed.
func TestDeleteTemplateRefusesEverythingElse(t *testing.T) {
	for _, name := range []string{"vm1", "vm1-2", bldLV, "foreign", "nope", "thin-test", "vg-test", "img1.qcow2"} {
		m, h, _ := newTemplateHost(t, mixedReport())
		_, err := m.DeleteTemplate(name, "")
		var ae *app.Error
		if !app.AsError(err, &ae) || ae.Kind != app.KindNotFound || !errors.Is(err, vmtool.ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
		if w := h.writes(t); len(w) != 0 {
			t.Errorf("%s: helper writes %q", name, w)
		}
	}
	// path-like names never reach the helper either
	m, h, _ := newTemplateHost(t, mixedReport())
	for _, name := range []string{"", "../vm1", "/dev/vg-test/vm1", "vg-test/vm1", "a b"} {
		if _, err := m.DeleteTemplate(name, ""); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
	if w := h.writes(t); len(w) != 0 {
		t.Errorf("helper writes %q", w)
	}
}

// A volume that stops being a template between the first read and the
// removal (the report is read again under the lock) is not removed.
func TestDeleteTemplateReReadsUnderTheLock(t *testing.T) {
	m, h, _ := newTemplateHost(t, mixedReport())
	// the first report is the mixed one; by the time the lock is held the
	// template has been replaced by an untagged LV of the same name
	h.write(t, "after.json", reportOf(poolRow("25.00"), thinLV(tplLV, 10*gib, "")))
	h.write(t, "on_report", `if [ ! -f "$ST/seen" ]; then touch "$ST/seen"; else cp "$ST/after.json" "$ST/report.json"; fi`)
	_, err := m.DeleteTemplate(tplLV, "")
	var ae *app.Error
	if !app.AsError(err, &ae) || ae.Kind != app.KindNotFound {
		t.Fatalf("err = %v, want not found", err)
	}
	if w := h.writes(t); len(w) != 0 {
		t.Errorf("helper writes %q", w)
	}
}

// delete takes the template's build lock: it waits for a build of the same
// (VG, template LV).
func TestDeleteTemplateTakesTheBuildLock(t *testing.T) {
	m, h, _ := newTemplateHost(t, mixedReport())
	unlock := m.Builds.lock("vg-test/" + tplLV)
	done := make(chan error, 1)
	go func() { _, err := m.DeleteTemplate(tplLV, ""); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("delete ran while the build lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if w := h.writes(t); len(w) != 0 {
		t.Fatalf("helper writes while the lock was held: %q", w)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "write verbs", h.writes(t), []string{"remove vg-test " + tplLV})
}

// Prune removes the stale image templates and nothing else: not the current
// template, the saved one, a clone, a building volume or a foreign LV.
func TestPruneRemovesOnlyStaleImageTemplates(t *testing.T) {
	m, h, _ := newTemplateHost(t, mixedReport())
	svc := &app.Service{}
	removed, err := svc.PruneTemplates(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %+v", removed)
	}
	eqStrings(t, "write verbs", h.writes(t), []string{
		"remove vg-test " + staleLV,
		"remove vg-test " + oldLV,
	})
}

// withImage gives the inner Manager an ImagePath that finds the cached file.
type withImage struct {
	*thinInner
	path string
}

func (w withImage) ImagePath(string) (string, error) { return w.path, nil }

func TestPrepareTemplateWarmWritesNothing(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, warmReport())
	c := newCache(t, uint64(10*gib), sha64a)
	m := newThinFor(t, withImage{&thinInner{}, c.img}, c)
	got, err := m.PrepareTemplate("img1", "thin-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.LV != tplLV || got.Stale || got.Name != "img1" {
		t.Errorf("answer %+v", got)
	}
	eqStrings(t, "helper argv", h.calls(t), []string{"report"})
}

func TestPrepareTemplateColdBuilds(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00")))
	h.write(t, "report_after.json", warmReport())
	h.write(t, "on_rename", `cp "$ST/report_after.json" "$ST/report.json"`)
	virtual := uint64(10*gib - 1<<20)
	c := newCache(t, virtual, sha64a)
	m := newThinFor(t, withImage{&thinInner{}, c.img}, c)

	got, err := m.PrepareTemplate("img1", "thin-test")
	if err != nil || got.LV != tplLV {
		t.Fatalf("PrepareTemplate = %+v, %v", got, err)
	}
	eqStrings(t, "helper argv", h.calls(t), []string{
		"report",
		"report",
		fmt.Sprintf("create vg-test thin-test %s %d vmtool_building", bldLV, virtual),
		"import vg-test " + bldLV + " " + c.img,
		"tag vg-test " + bldLV + " +vmtool_template +vmtool_name=img1 +vmtool_sha256=" + sha64a + " +vmtool_origin=image -vmtool_building",
		"rename vg-test " + bldLV + " " + tplLV,
		"report",
	})
}

// Only the template is promised, not a clone: with 225 GiB already promised
// in a 60 GiB pool (limit 240 GiB) a 10 GiB template fits, template plus a
// 10 GiB clone (what create checks) does not.
func TestPrepareTemplateRoomCheckIsForTheTemplateOnly(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vmx", 225*gib, cloneTags)))
	h.write(t, "report_after.json", reportOf(poolRow("25.00"), thinLV("vmx", 225*gib, cloneTags), thinLV(tplLV, 10*gib, tplTagsA)))
	h.write(t, "on_rename", `cp "$ST/report_after.json" "$ST/report.json"`)
	c := newCache(t, uint64(10*gib), sha64a)
	m := newThinFor(t, withImage{&thinInner{}, c.img}, c)
	if _, err := m.PrepareTemplate("img1", "thin-test"); err != nil {
		t.Fatalf("a template that fits was refused: %v", err)
	}
	if h.count(t, "create") != 1 {
		t.Errorf("calls %q", h.calls(t))
	}

	// and a pool with no room for it is a conflict, before any write
	h2 := newHelperStub(t)
	h2.report(t, reportOf(poolRow("25.00"), thinLV("vmx", 235*gib, cloneTags)))
	m = newThinFor(t, withImage{&thinInner{}, c.img}, c)
	_, err := m.PrepareTemplate("img1", "thin-test")
	var ae *app.Error
	if !app.AsError(err, &ae) || ae.Kind != app.KindConflict || len(h2.writes(t)) != 0 {
		t.Errorf("no room: %v writes %q", err, h2.writes(t))
	}
}

func TestPrepareTemplateRefusals(t *testing.T) {
	cases := []struct {
		name, image, pool string
		kind              app.Kind
		inner             func(c cache) app.Manager
	}{
		{"not a thin pool", "img1", "vmtool", app.KindInvalid, nil},
		{"unknown pool", "img1", "nope", app.KindInvalid, nil},
		{"no pool given", "img1", "", app.KindInvalid, nil},
		{"bad image name", "../img1", "thin-test", app.KindInvalid, nil},
		{"image not on the host", "img1", "thin-test", app.KindNotFound, func(cache) app.Manager { return &thinInner{} }}, // thinInner's ImagePath is not found
	}
	for _, tc := range cases {
		h := newHelperStub(t)
		h.report(t, reportOf(poolRow("25.00")))
		c := newCache(t, uint64(10*gib), sha64a)
		var inner app.Manager = withImage{&thinInner{}, c.img}
		if tc.inner != nil {
			inner = tc.inner(c)
		}
		m := newThinFor(t, inner, c)
		_, err := m.PrepareTemplate(tc.image, tc.pool)
		var ae *app.Error
		if tc.kind == app.KindNotFound {
			if !vmtool.IsNotFound(err) {
				t.Errorf("%s: %v, want not found", tc.name, err)
			}
		} else if !app.AsError(err, &ae) || ae.Kind != tc.kind {
			t.Errorf("%s: %v, want kind %v", tc.name, err, tc.kind)
		}
		if w := h.writes(t); len(w) != 0 {
			t.Errorf("%s: helper writes %q", tc.name, w)
		}
	}
	// an image the manifest does not list cannot be built from
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00")))
	c := newCache(t, uint64(10*gib), sha64a)
	m := newThinFor(t, withImage{&thinInner{}, c.dir + "/unlisted"}, c)
	_, err := m.PrepareTemplate("unlisted", "thin-test")
	var ae *app.Error
	if !app.AsError(err, &ae) || ae.Kind != app.KindInvalid || len(h.writes(t)) != 0 {
		t.Errorf("unlisted image: %v writes %q", err, h.writes(t))
	}
}

// The wrappers keep the interface visible and apply the pool defaults.
type tmplRec struct {
	app.Manager
	got []string
}

func (r *tmplRec) ListTemplates() ([]app.TemplateInfo, error) {
	r.got = append(r.got, "list")
	return []app.TemplateInfo{{Template: vmtool.Template{Name: "x"}}}, nil
}
func (r *tmplRec) PrepareTemplate(image, pool string) (app.TemplateInfo, error) {
	r.got = append(r.got, "prepare "+image+" "+pool)
	return app.TemplateInfo{}, nil
}
func (r *tmplRec) DeleteTemplate(name, pool string) (app.TemplateInfo, error) {
	r.got = append(r.got, "delete "+name+" "+pool)
	return app.TemplateInfo{}, nil
}

func TestWrappersForwardTemplateManager(t *testing.T) {
	// workstation: defaultPools alone
	rec := &tmplRec{}
	var w app.Manager = withDefaultPools(rec, "thin-test")
	tm, ok := w.(app.TemplateManager)
	if !ok {
		t.Fatal("withDefaultPools hides app.TemplateManager")
	}
	if l, err := tm.ListTemplates(); err != nil || len(l) != 1 {
		t.Fatal(l, err)
	}
	tm.PrepareTemplate("img1", "")
	tm.PrepareTemplate("img1", "other")
	tm.DeleteTemplate("img1", "")
	eqStrings(t, "through defaultPools", rec.got, []string{"list", "prepare img1 thin-test", "prepare img1 other", "delete img1 "})

	// hypervisor: hypervisorPools, an empty pool is the vmtool pool
	rec = &tmplRec{}
	hp := hypervisorPools{rec}
	hp.PrepareTemplate("img1", "")
	hp.PrepareTemplate("img1", "thin-test")
	eqStrings(t, "through hypervisorPools", rec.got, []string{"prepare img1 vmtool", "prepare img1 thin-test"})

	// over a Manager without templates they say so, they do not panic
	plain := hypervisorPools{&thinInner{}}
	if l, err := plain.ListTemplates(); err == nil || !errors.Is(err, app.ErrNoThinPools) || l != nil {
		t.Errorf("list over a plain Manager: %v %v", l, err)
	}
}
