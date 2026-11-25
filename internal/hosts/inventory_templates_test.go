package hosts

import (
	"fmt"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

func templateReader(t *testing.T, thin *vmtool.Thin) app.InventoryTemplateReader {
	t.Helper()
	m := invMgr{}
	r := NewWith("hv1", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return m, func() {}, nil })
	if thin != nil {
		r = r.WithThin(thin)
	}
	rd, done, err := r.InventorySource().Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(done)
	tr, ok := rd.(app.InventoryTemplateReader)
	if !ok {
		t.Fatalf("the reader %T does not list templates", rd)
	}
	return tr
}

// The reporter's template list is what the helper's report shows: image and
// saved templates only (no building volume, no clone, no foreign LV), with
// the daemon's own fields.
func TestInventoryReaderListsTemplates(t *testing.T) {
	h := newHelperStub(t)
	timed := fmt.Sprintf(`{"lv_name":%q,"vg_name":"vg-test","lv_attr":"Vwi-aotz--","lv_size":"%d","pool_lv":"thin-test","origin":"","data_percent":"","metadata_percent":"","lv_tags":%q,"lv_time":"2026-11-20 01:02:03 +0000"}`,
		tplLV, 10*gib, tplTagsA)
	h.report(t, reportOf(poolRow("25.00"),
		timed,
		thinLV(savedLV, 5*gib, savedTags),
		cloneLV("vm1", tplLV),
		thinLV(bldLV, gib, "vmtool_building"),
		thinLV("foreign", gib, ""),
	))
	got, err := templateReader(t, &vmtool.Thin{Helper: thinHelper}).Templates()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d templates, want 2: %+v", len(got), got)
	}
	img, saved := got[0], got[1]
	if img.Name != "img1" || img.Image != "img1" || img.Origin != vmtool.TemplateOriginImage || img.Pool != "thin-test" || img.SizeBytes != 10*gib {
		t.Errorf("image template: %+v", img)
	}
	if saved.Name != "proj" || saved.Image != "" || saved.Origin != vmtool.TemplateOriginSaved || saved.Pool != "thin-test" || saved.SizeBytes != 5*gib {
		t.Errorf("saved template: %+v", saved)
	}
	if !img.Created.Equal(time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)) {
		t.Errorf("created %v", img.Created)
	}
	if !saved.Created.IsZero() {
		t.Errorf("a template the helper gave no time for has created %v", saved.Created)
	}
}

// A host with no thin pools has no templates: an empty list, not an error.
func TestInventoryReaderNoThinHasNoTemplates(t *testing.T) {
	got, err := templateReader(t, nil).Templates()
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

// A helper that looked and found no thin pool (no volume group, or no LVM on
// the host) prints the empty report: the host has no templates, an empty
// list and not an error, so the reporter sends it and the store marks the
// host's templates as reported.
func TestInventoryReaderEmptyReportHasNoTemplates(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, `{"report": [{"lv": []}]}`)
	got, err := templateReader(t, &vmtool.Thin{Helper: thinHelper}).Templates()
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

// The template listing shares the helper's report: the empty report lists
// no templates (an empty list, not an error).
func TestListTemplatesEmptyReportIsEmpty(t *testing.T) {
	m, _, _ := newTemplateHost(t, `{"report": [{"lv": []}]}`)
	got, err := m.ListTemplates()
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
}

// A helper that cannot run is an error: the reporter then sends no template
// list and the store keeps its last one.
func TestInventoryReaderTemplatesHelperFails(t *testing.T) {
	h := newHelperStub(t)
	h.write(t, "fail_report", "no helper here")
	if got, err := templateReader(t, &vmtool.Thin{Helper: thinHelper}).Templates(); err == nil {
		t.Fatalf("want an error, got %+v", got)
	}
}
