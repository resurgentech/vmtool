package hosts

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Export volumes on the daemon side (tranche 13, E2). The helper is the stub
// sudo of thinclone_test.go; no real lvm, qemu-img or sudo runs.

const (
	expLV    = "vmexp-000000000001"
	expLV2   = "vmexp-000000000002"
	expLV3   = "vmexp-000000000003"
	expTime  = "2026-11-20 12:00:00 +0000"
	expFrom  = "vmtool_export,vmtool_name=proj,vmtool_from=" + savedLV
	expDone  = expFrom + ",vmtool_bytes=10"
	nameRE   = `vmexp-[0-9a-f]{12}`
	roomLine = "vmtool-thin: no room: need=21474836480 free=40000000000 reserve=30000000000"
)

// exportRow is a thick export LV's lvs row with an lv_time.
func exportRow(name, tags, when string) string {
	return fmt.Sprintf(`{"lv_name":%q,"vg_name":"vg-test","lv_attr":"-wi-a-----","lv_size":"%d","pool_lv":"","origin":"","data_percent":"","metadata_percent":"","lv_tags":%q,"lv_time":%q}`,
		name, 10*gib, tags, when)
}

func reportWithExports(rows ...string) string {
	all := append([]string{poolRow("25.00"), thinLV(savedLV, 10*gib, savedTags), thinLV("vm1", 20*gib, "vmtool_vol")}, rows...)
	return reportOf(all...)
}

func lookupProj(t *testing.T, x app.TemplateVolumeExporter) app.TemplateInfo {
	t.Helper()
	tpl, err := x.LookupTemplate("proj", "")
	if err != nil {
		t.Fatal(err)
	}
	return tpl
}

// after.json is the report once the helper's export has made the volume; the
// stub swaps in the name the daemon chose.
func (h *helperStub) exportMakes(t *testing.T, tags string) {
	t.Helper()
	h.write(t, "after.json", reportWithExports(exportRow("NAMEPLACEHOLDER", tags, expTime)))
	h.write(t, "on_export", `printf '    (50.00/100%%)\r    (100.00/100%%)\r'; sed "s/NAMEPLACEHOLDER/$3/" "$ST/after.json" > "$ST/report.json"`)
}

func TestExportTemplateVolumeMakesAnExportVolume(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports())
	h.exportMakes(t, expDone)
	x := exporter(t, newThinFor(t, &thinInner{}, cache{}))
	tpl := lookupProj(t, x)

	var pcts []float64
	ev, err := x.ExportTemplateVolume(tpl, func(p float64) { pcts = append(pcts, p) })
	if err != nil {
		t.Fatalf("ExportTemplateVolume: %v", err)
	}
	w := h.writes(t)
	if len(w) != 1 {
		t.Fatalf("helper writes = %q", w)
	}
	re := regexp.MustCompile(`^export vg-test ` + savedLV + ` (` + nameRE + `) vmtool_export vmtool_name=proj vmtool_from=` + savedLV + `$`)
	m := re.FindStringSubmatch(w[0])
	if m == nil {
		t.Fatalf("export call = %q", w[0])
	}
	if ev.Name != m[1] || ev.VG != "vg-test" || ev.Bytes != 10 || ev.Reused || ev.SHA256 != "" {
		t.Errorf("volume = %+v", ev)
	}
	if len(pcts) != 2 || pcts[0] != 50 || pcts[1] != 100 {
		t.Errorf("progress = %v", pcts)
	}
}

func TestExportTemplateVolumeReusesAnExport(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports(exportRow(expLV, expDone+",vmtool_sha256="+sha64a, expTime)))
	x := exporter(t, newThinFor(t, &thinInner{}, cache{}))

	ev, err := x.ExportTemplateVolume(lookupProj(t, x), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name != expLV || ev.Bytes != 10 || ev.SHA256 != sha64a || !ev.Reused || !ev.Created.Equal(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("volume = %+v", ev)
	}
	if n := len(h.writes(t)); n != 0 {
		t.Errorf("a reuse wrote: %q", h.writes(t))
	}
}

// An export with no vmtool_bytes (a crash left it) is removed and redone; one
// of a template of this name that was replaced is removed first as well.
func TestExportTemplateVolumeClearsIncompleteAndStaleExports(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports(
		exportRow(expLV, expFrom, expTime), // incomplete, of this template
		exportRow(expLV2, "vmtool_export,vmtool_name=proj,vmtool_from=vmtpl-0000000000aa,vmtool_bytes=5", expTime),  // template replaced
		exportRow(expLV3, "vmtool_export,vmtool_name=other,vmtool_from=vmtpl-0000000000bb,vmtool_bytes=5", expTime), // someone else's
	))
	h.exportMakes(t, expDone)
	x := exporter(t, newThinFor(t, &thinInner{}, cache{}))

	if _, err := x.ExportTemplateVolume(lookupProj(t, x), nil); err != nil {
		t.Fatal(err)
	}
	w := h.writes(t)
	if len(w) != 3 || w[0] != "remove vg-test "+expLV || w[1] != "remove vg-test "+expLV2 || !strings.HasPrefix(w[2], "export vg-test "+savedLV+" ") {
		t.Fatalf("helper writes = %q", w)
	}
}

func TestExportTemplateVolumeNoRoomIsAConflict(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports())
	h.write(t, "fail_export", roomLine)
	x := exporter(t, newThinFor(t, &thinInner{}, cache{}))

	_, err := x.ExportTemplateVolume(lookupProj(t, x), nil)
	var ae *app.Error
	if !errors.As(err, &ae) || ae.Kind != app.KindConflict || !errors.Is(err, vmtool.ErrThinNoRoom) {
		t.Fatalf("err = %v, want a conflict matching ErrThinNoRoom", err)
	}
	for _, want := range []string{"proj", "vg-test", "nothing was written"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
}

func TestExportTemplateVolumeFailureIsReported(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports())
	h.write(t, "fail_export", "vmtool-thin: convert failed")
	x := exporter(t, newThinFor(t, &thinInner{}, cache{}))
	_, err := x.ExportTemplateVolume(lookupProj(t, x), nil)
	if err == nil || !strings.Contains(err.Error(), "convert failed") || errors.Is(err, vmtool.ErrThinNoRoom) {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenExportReadsExactlyTheTaggedBytes(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports(exportRow(expLV, expDone+",vmtool_sha256="+sha64a, expTime)))
	h.write(t, "on_send", `printf 0123456789`)
	x := exporter(t, newThinFor(t, &thinInner{}, cache{}))

	r, ev, err := x.OpenExport(expLV)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(b) != "0123456789" {
		t.Fatalf("read %q, %v", b, err)
	}
	if ev.Name != expLV || ev.Bytes != 10 || ev.SHA256 != sha64a {
		t.Errorf("volume = %+v", ev)
	}
	eqStrings(t, "writes", h.writes(t), []string{"send vg-test " + expLV})

	// a helper that gives fewer bytes than the tag is an error, not an EOF
	h.write(t, "on_send", `printf 01234`)
	r, _, err = x.OpenExport(expLV)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); err == nil {
		t.Error("a short send read as a clean EOF")
	}
	r.Close()
}

func TestOpenExportRefuses(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports(exportRow(expLV, expFrom, expTime))) // incomplete
	x := exporter(t, newThinFor(t, &thinInner{}, cache{}))
	if _, _, err := x.OpenExport(expLV2); !vmtool.IsNotFound(err) {
		t.Errorf("unknown export = %v, want not found", err)
	}
	if _, _, err := x.OpenExport(expLV); err == nil || vmtool.IsNotFound(err) {
		t.Errorf("incomplete export = %v, want a refusal that is not not-found", err)
	}
	for _, bad := range []string{"vm1", savedLV, "../x", ""} {
		if _, _, err := x.OpenExport(bad); err == nil {
			t.Errorf("OpenExport(%q) succeeded", bad)
		}
	}
	if n := h.count(t, "send"); n != 0 {
		t.Errorf("send ran %d times", n)
	}
}

func TestRecordTemplateSHAAlsoTagsTheExport(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports(exportRow(expLV, expDone, expTime)))
	x := exporter(t, newThinFor(t, &thinInner{}, cache{}))
	if err := x.RecordTemplateSHA(lookupProj(t, x), sha64a); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "tags", h.writes(t), []string{
		"tag vg-test " + savedLV + " +vmtool_sha256=" + sha64a,
		"tag vg-test " + expLV + " +vmtool_sha256=" + sha64a,
	})

	// the old digest on both is replaced in one call each
	h2 := newHelperStub(t)
	h2.report(t, reportOf(poolRow("25.00"), thinLV(savedLV, 10*gib, savedTags+",vmtool_sha256="+sha64b),
		exportRow(expLV, expDone+",vmtool_sha256="+sha64b, expTime)))
	x = exporter(t, newThinFor(t, &thinInner{}, cache{}))
	if err := x.RecordTemplateSHA(lookupProj(t, x), sha64a); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "replace", h2.writes(t), []string{
		"tag vg-test " + savedLV + " -vmtool_sha256=" + sha64b + " +vmtool_sha256=" + sha64a,
		"tag vg-test " + expLV + " -vmtool_sha256=" + sha64b + " +vmtool_sha256=" + sha64a,
	})

	// the template already has it, the export does not: only the export is tagged
	h3 := newHelperStub(t)
	h3.report(t, reportOf(poolRow("25.00"), thinLV(savedLV, 10*gib, savedTags+",vmtool_sha256="+sha64a),
		exportRow(expLV, expDone, expTime)))
	x = exporter(t, newThinFor(t, &thinInner{}, cache{}))
	if err := x.RecordTemplateSHA(lookupProj(t, x), sha64a); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "export only", h3.writes(t), []string{"tag vg-test " + expLV + " +vmtool_sha256=" + sha64a})

	// no export: as before
	h4 := newHelperStub(t)
	h4.report(t, reportWithExports())
	x = exporter(t, newThinFor(t, &thinInner{}, cache{}))
	if err := x.RecordTemplateSHA(lookupProj(t, x), sha64a); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "no export", h4.writes(t), []string{"tag vg-test " + savedLV + " +vmtool_sha256=" + sha64a})
}

func TestListTemplatesShowsTheExport(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports(exportRow(expLV, expDone+",vmtool_sha256="+sha64a, expTime)))
	m := newThinFor(t, &thinInner{}, newCache(t, uint64(gib), sha64a))
	got, err := m.ListTemplates()
	if err != nil || len(got) != 1 {
		t.Fatalf("ListTemplates = %+v, %v", got, err)
	}
	e := got[0].Export
	if e == nil || e.Name != expLV || e.Bytes != 10 || e.SHA256 != sha64a {
		t.Fatalf("export = %+v", e)
	}
	if want := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC); !e.Expires().Equal(want) {
		t.Errorf("Expires = %v, want %v", e.Expires(), want)
	}
	// a template with no export has none
	h.report(t, reportWithExports())
	got, _ = m.ListTemplates()
	if len(got) != 1 || got[0].Export != nil {
		t.Errorf("export = %+v", got)
	}
}

func TestDeleteTemplateRemovesItsExport(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports(exportRow(expLV, expDone, expTime), exportRow(expLV3, "vmtool_export,vmtool_name=other,vmtool_from=vmtpl-0000000000bb,vmtool_bytes=5", expTime)))
	m := newThinFor(t, &thinInner{}, newCache(t, uint64(gib), sha64a))
	if _, err := m.DeleteTemplate("proj", ""); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "writes", h.writes(t), []string{"remove vg-test " + savedLV, "remove vg-test " + expLV})
}

// A failed export removal does not undo the template's removal: it is logged,
// and the hourly expiry retries it.
func TestDeleteTemplateExportRemovalFailureIsLogged(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports(exportRow(expLV, expDone, expTime)))
	h.write(t, "on_remove", `case "$2" in vmexp-*) echo "vmtool-thin: busy" >&2; exit 1;; esac`)
	var buf bytes.Buffer
	m := newThinFor(t, &thinInner{}, newCache(t, uint64(gib), sha64a))
	m.Log = logger(&buf)
	if _, err := m.DeleteTemplate("proj", ""); err != nil {
		t.Fatalf("DeleteTemplate: %v", err)
	}
	if !strings.Contains(buf.String(), expLV) || !strings.Contains(buf.String(), "busy") {
		t.Errorf("log = %q", buf.String())
	}
}

func TestExpireExports(t *testing.T) {
	old := "2026-11-19 11:00:00 +0000"   // 25 h before the clock
	young := "2026-11-19 13:00:00 +0000" // 23 h before the clock
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	h := newHelperStub(t)
	h.report(t, reportWithExports(
		exportRow(expLV, expDone, old),
		exportRow(expLV2, expDone, young),
		exportRow(expLV3, expFrom, old),              // incomplete and old: expires like the rest
		exportRow("vmexp-000000000004", expDone, ""), // no lv_time: never guessed at
	))
	var buf bytes.Buffer
	m := newThinFor(t, &thinInner{}, cache{})
	m.Log = logger(&buf)
	if n := m.ExpireExports(now); n != 2 {
		t.Errorf("removed %d, want 2", n)
	}
	eqStrings(t, "writes", h.writes(t), []string{"remove vg-test " + expLV, "remove vg-test " + expLV3})
	if buf.Len() != 0 {
		t.Errorf("a clean expiry logged: %q", buf.String())
	}

	// exactly 24 h is not yet over; a minute later is
	if n := m.ExpireExports(now.Add(-time.Hour)); n != 0 {
		t.Errorf("an early clock removed %d", n)
	}
	h2 := newHelperStub(t)
	h2.report(t, reportWithExports(exportRow(expLV, expDone, expTime)))
	m = newThinFor(t, &thinInner{}, cache{})
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	if n := m.ExpireExports(at); n != 0 {
		t.Errorf("at exactly 24 h removed %d", n)
	}
	if n := m.ExpireExports(at.Add(time.Minute)); n != 1 {
		t.Errorf("after 24 h removed %d", n)
	}
}

func TestExpireExportsRetriesAFailedRemoval(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	h := newHelperStub(t)
	h.report(t, reportWithExports(exportRow(expLV, expDone, expTime)))
	h.write(t, "fail_remove", "vmtool-thin: busy")
	var buf bytes.Buffer
	m := newThinFor(t, &thinInner{}, cache{})
	m.Log = logger(&buf)
	if n := m.ExpireExports(now); n != 0 {
		t.Errorf("removed %d with a failing helper", n)
	}
	if !strings.Contains(buf.String(), expLV) || !strings.Contains(buf.String(), "busy") {
		t.Errorf("failure not logged: %q", buf.String())
	}
	// the next hour: the helper works, the same volume goes
	if err := os.Remove(h.dir + "/fail_remove"); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if n := m.ExpireExports(now.Add(time.Hour)); n != 1 {
		t.Errorf("retry removed %d", n)
	}
	if buf.Len() != 0 {
		t.Errorf("a clean retry logged: %q", buf.String())
	}
	if got := h.count(t, "remove"); got != 2 {
		t.Errorf("remove calls = %d, want 2", got)
	}
}

func TestExpireExportsHelperDownIsOneLogLine(t *testing.T) {
	h := newHelperStub(t)
	h.write(t, "fail_report", "vmtool-thin: no lvm")
	var buf bytes.Buffer
	m := newThinFor(t, &thinInner{}, cache{})
	m.Log = logger(&buf)
	if n := m.ExpireExports(time.Now()); n != 0 {
		t.Errorf("removed %d", n)
	}
	if got := strings.Count(strings.TrimSpace(buf.String()), "\n"); buf.Len() == 0 || got != 0 {
		t.Errorf("log = %q", buf.String())
	}
}

// A template's name finds its finished export: what `images pull host/<name>`
// asks for. An unfinished export is not found by name.
func TestOpenExportByTemplateName(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportWithExports(exportRow(expLV, expDone, expTime)))
	h.write(t, "on_send", `printf 0123456789`)
	x := exporter(t, newThinFor(t, &thinInner{}, cache{}))
	r, ev, err := x.OpenExport("proj")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "0123456789" || ev.Name != expLV || ev.TemplateName != "proj" || ev.From != savedLV {
		t.Errorf("read %q, volume %+v", b, ev)
	}
	eqStrings(t, "writes", h.writes(t), []string{"send vg-test " + expLV})

	h2 := newHelperStub(t)
	h2.report(t, reportWithExports(exportRow(expLV, expFrom, expTime))) // unfinished
	x = exporter(t, newThinFor(t, &thinInner{}, cache{}))
	if _, _, err := x.OpenExport("proj"); !vmtool.IsNotFound(err) {
		t.Errorf("an unfinished export by name = %v, want not found", err)
	}
	if n := h2.count(t, "send"); n != 0 {
		t.Errorf("send ran %d times", n)
	}
}
