package vmtool

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/testexec"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tGiB = int64(1) << 30
)

// lvJSON renders one lvs row (every value a string, as lvs prints them).
func lvJSON(name, vg, attr, size, pool, origin, data, meta, tags string) string {
	return `{"lv_name":"` + name + `","vg_name":"` + vg + `","lv_attr":"` + attr +
		`","lv_size":"` + size + `","pool_lv":"` + pool + `","origin":"` + origin +
		`","data_percent":"` + data + `","metadata_percent":"` + meta +
		`","lv_tags":"` + tags + `","lv_time":"2026-11-18 12:00:00 +0000","lv_future":"x"}`
}

func reportJSON(rows ...string) string {
	return `{"report":[{"lv":[` + strings.Join(rows, ",") + `]}]}`
}

const (
	sz60 = "64424509440"
	sz20 = "21474836480"
	sz10 = "10737418240"
)

func fixtureReport() string {
	return reportJSON(
		lvJSON("thin-test", "vg-test", "twi-aotz--", sz60, "", "", "12.34", "1.00", "vmtool_thinpool"),
		lvJSON("vm1", "vg-test", "Vwi-aotz--", sz20, "thin-test", "vmtpl-000000000001", "5.00", "", "vmtool_vol"),
		lvJSON("vm1-2", "vg-test", "Vwi-aotz--", sz10, "thin-test", "", "", "", "vmtool_vol"),
		lvJSON("vmtpl-000000000001", "vg-test", "Vwi-a-tz--", sz10, "thin-test", "", "50.00", "",
			"vmtool_template,vmtool_name=img1,vmtool_sha256="+shaA+",vmtool_origin=image"),
		lvJSON("vmtpl-000000000002", "vg-test", "Vwi-a-tz--", sz10, "thin-test", "", "", "", "vmtool_building"),
		// a foreign thin LV in the pool: counts toward promised size, is not listed
		lvJSON("foreign", "vg-test", "Vwi-a-tz--", sz10, "thin-test", "", "1.00", "", "other"),
		// a thick LV, a hidden LV, and a thin LV in a pool vmtool does not own
		lvJSON("thick1", "vg-test", "-wi-ao----", sz10, "", "", "", "", "vmtool_vol"),
		lvJSON("[thin-test_tdata]", "vg-test", "Twi-ao----", sz60, "", "", "", "", ""),
		lvJSON("[lvol0_pmspare]", "vg-test", "ewi-------", sz10, "", "", "", "", ""),
		lvJSON("otherpool", "vg-test", "twi-a-tz--", sz60, "", "", "0.00", "0.00", ""),
		lvJSON("alien", "vg-test", "Vwi-a-tz--", sz10, "otherpool", "", "", "", "vmtool_vol"),
	)
}

func TestThinParseReport(t *testing.T) {
	pools, vols, err := ParseThinReport([]byte(fixtureReport()))
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 {
		t.Fatalf("pools = %+v", pools)
	}
	p := pools[0]
	// virtual = vm1 20G + vm1-2 10G + template 10G + building 10G + foreign 10G
	want := ThinPool{Name: "thin-test", VG: "vg-test", SizeBytes: 60 * tGiB, DataPercent: 12.34, MetaPercent: 1.0, VirtualBytes: 60 * tGiB}
	if p != want {
		t.Fatalf("pool = %+v, want %+v", p, want)
	}
	kinds := map[string]string{}
	for _, v := range vols {
		kinds[v.Name] = v.Kind
	}
	wantKinds := map[string]string{
		"vm1": ThinKindVol, "vm1-2": ThinKindVol,
		"vmtpl-000000000001": ThinKindTemplate, "vmtpl-000000000002": ThinKindBuilding,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("kinds = %v, want %v", kinds, wantKinds)
	}
	var vm1 ThinVolume
	for _, v := range vols {
		if v.Name == "vm1" {
			vm1 = v
		}
	}
	if vm1.VG != "vg-test" || vm1.Pool != "thin-test" || vm1.SizeBytes != 20*tGiB ||
		vm1.Origin != "vmtpl-000000000001" || vm1.DataPercent != 5.0 ||
		!reflect.DeepEqual(vm1.Tags, []string{"vmtool_vol"}) ||
		!vm1.Created.Equal(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("vm1 = %+v", vm1)
	}
}

func TestThinParseReportKindPrecedence(t *testing.T) {
	j := reportJSON(
		lvJSON("thin-test", "vg-test", "twi-aotz--", sz60, "", "", "0", "0", "vmtool_thinpool"),
		lvJSON("a", "vg-test", "Vwi-a-tz--", sz10, "thin-test", "", "", "", "vmtool_vol,vmtool_template"),
		lvJSON("b", "vg-test", "Vwi-a-tz--", sz10, "thin-test", "", "", "", "vmtool_template,vmtool_building"),
		lvJSON("c", "vg-test", "Vwi-a-tz--", sz10, "thin-test", "", "", "", "vmtool_vol_extra"),
	)
	_, vols, err := ParseThinReport([]byte(j))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range vols {
		got[v.Name] = v.Kind
	}
	// building beats template beats vol; a tag that only starts like one is no tag.
	want := map[string]string{"a": ThinKindTemplate, "b": ThinKindBuilding}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestThinParseReportTwoVGsAndElements(t *testing.T) {
	j := `{"report":[{"lv":[` +
		lvJSON("thin-test", "vg-test", "twi-aotz--", sz60, "", "", "1", "1", "vmtool_thinpool") + `,` +
		lvJSON("vm1", "vg-test", "Vwi-a-tz--", sz10, "thin-test", "", "", "", "vmtool_vol") +
		`]},{"lv":[` +
		lvJSON("thin-test", "vg-two", "twi-aotz--", sz20, "", "", "2", "2", "vmtool_thinpool") + `,` +
		lvJSON("vm1", "vg-two", "Vwi-a-tz--", sz10, "thin-test", "", "", "", "vmtool_vol") + `,` +
		// same pool name in the other VG must not count toward vg-test
		lvJSON("vm9", "vg-two", "Vwi-a-tz--", sz10, "thin-test", "", "", "", "vmtool_vol") +
		`]}]}`
	pools, vols, err := ParseThinReport([]byte(j))
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 2 || len(vols) != 3 {
		t.Fatalf("pools=%+v vols=%+v", pools, vols)
	}
	for _, p := range pools {
		switch p.VG {
		case "vg-test":
			if p.VirtualBytes != 10*tGiB {
				t.Errorf("vg-test virtual = %d", p.VirtualBytes)
			}
		case "vg-two":
			if p.VirtualBytes != 20*tGiB {
				t.Errorf("vg-two virtual = %d", p.VirtualBytes)
			}
		default:
			t.Errorf("unexpected pool %+v", p)
		}
	}
}

func TestThinParseReportEmpty(t *testing.T) {
	for _, j := range []string{`{"report": [{"lv": []}]}`, `{"report":[]}`, `{}`} {
		pools, vols, err := ParseThinReport([]byte(j))
		if err != nil || len(pools) != 0 || len(vols) != 0 {
			t.Errorf("%s: pools=%v vols=%v err=%v", j, pools, vols, err)
		}
	}
}

func TestThinParseReportErrors(t *testing.T) {
	bad := map[string]string{
		"not json":  `thin-test|vg`,
		"truncated": `{"report":[{"lv":[`,
		"bad size on a kept volume": reportJSON(
			lvJSON("thin-test", "vg-test", "twi-aotz--", sz60, "", "", "0", "0", "vmtool_thinpool"),
			lvJSON("vm1", "vg-test", "Vwi-a-tz--", "20G", "thin-test", "", "", "", "vmtool_vol")),
		"bad size on the pool": reportJSON(
			lvJSON("thin-test", "vg-test", "twi-aotz--", "", "", "", "0", "0", "vmtool_thinpool")),
		"bad size on a foreign thin lv in the pool": reportJSON(
			lvJSON("thin-test", "vg-test", "twi-aotz--", sz60, "", "", "0", "0", "vmtool_thinpool"),
			lvJSON("foreign", "vg-test", "Vwi-a-tz--", "1.5", "thin-test", "", "", "", "")),
		"bad percent": reportJSON(
			lvJSON("thin-test", "vg-test", "twi-aotz--", sz60, "", "", "lots", "0", "vmtool_thinpool")),
	}
	for name, j := range bad {
		if _, _, err := ParseThinReport([]byte(j)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// a malformed size on an LV that is neither kept nor counted is ignored
	ok := reportJSON(
		lvJSON("thin-test", "vg-test", "twi-aotz--", sz60, "", "", "0", "0", "vmtool_thinpool"),
		lvJSON("thick1", "vg-test", "-wi-ao----", "?", "", "", "", "", ""))
	if _, _, err := ParseThinReport([]byte(ok)); err != nil {
		t.Errorf("ignored LV broke the parse: %v", err)
	}
}

func TestThinCheckRoom(t *testing.T) {
	pool := ThinPool{Name: "thin-test", VG: "vg-test", SizeBytes: 100 * tGiB, DataPercent: 10, MetaPercent: 10, VirtualBytes: 100 * tGiB}
	if err := CheckThinRoom(pool, 1*tGiB); err != nil {
		t.Fatalf("ok pool: %v", err)
	}
	// exactly 4x is allowed, 4x + 1 byte is not
	if err := CheckThinRoom(pool, 300*tGiB); err != nil {
		t.Fatalf("exactly 4x: %v", err)
	}
	err := CheckThinRoom(pool, 300*tGiB+1)
	if !errors.Is(err, ErrThinNoRoom) {
		t.Fatalf("4x+1 = %v", err)
	}
	for _, want := range []string{"thin-test", "400", "100.0 GiB", "4x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), "107374182400") || !strings.Contains(err.Error(), "429496729600") {
		t.Errorf("message %q lacks byte counts", err)
	}

	for _, tc := range []struct {
		data, meta float64
		refuse     bool
	}{
		{79.99, 0, false}, {80.00, 0, true}, {95, 0, true},
		{0, 79.99, false}, {0, 80.00, true}, {0, 99, true},
	} {
		p := pool
		p.DataPercent, p.MetaPercent = tc.data, tc.meta
		err := CheckThinRoom(p, tGiB)
		if (err != nil) != tc.refuse {
			t.Errorf("data %.2f meta %.2f: err = %v", tc.data, tc.meta, err)
		}
		if err != nil {
			if !errors.Is(err, ErrThinNoRoom) {
				t.Errorf("not the sentinel: %v", err)
			}
			if !strings.Contains(err.Error(), "80.00%") && !strings.Contains(err.Error(), "80%") {
				t.Errorf("message lacks the limit: %v", err)
			}
		}
	}
	if err := CheckThinRoom(pool, -1); err == nil || errors.Is(err, ErrThinNoRoom) {
		t.Errorf("negative add = %v, want a plain error", err)
	}
}

func TestThinLVName(t *testing.T) {
	got, err := TemplateLVName(shaA)
	if err != nil || got != "vmtpl-aaaaaaaaaaaa" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = TemplateLVName("0123456789ABCDEF")
	if err != nil || got != "vmtpl-0123456789ab" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "abc", "0123456789a", "0123456789ag-", "zzzzzzzzzzzzzzzz", "../../etc/pa"} {
		if _, err := TemplateLVName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestThinTemplateTags(t *testing.T) {
	tags, err := TemplateTags("img1", shaA, TemplateOriginImage)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"vmtool_template", "vmtool_name=img1", "vmtool_sha256=" + shaA, "vmtool_origin=image"}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("tags = %v", tags)
	}
	tags, err = TemplateTags("proj-1.base", "", TemplateOriginSaved)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"vmtool_template", "vmtool_name=proj-1.base", "vmtool_origin=saved"}
	if !reflect.DeepEqual(tags, want) {
		t.Fatalf("saved tags = %v", tags)
	}
	for _, c := range []struct{ name, sha, origin string }{
		{"", shaA, "image"}, {"has space", shaA, "image"}, {"a=b", shaA, "image"},
		{"a,b", shaA, "image"}, {"-x", shaA, "image"}, {"img1", "", "image"},
		{"img1", "xyz", "image"}, {"img1", strings.ToUpper(shaA), "image"},
		{"img1", shaA, "weird"}, {"img1", shaA, ""}, {"a/b", shaA, "image"},
	} {
		if _, err := TemplateTags(c.name, c.sha, c.origin); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestThinTemplatesFromVolumes(t *testing.T) {
	_, vols, err := ParseThinReport([]byte(fixtureReport()))
	if err != nil {
		t.Fatal(err)
	}
	ts := ThinTemplates(vols)
	if len(ts) != 1 {
		t.Fatalf("templates = %+v", ts)
	}
	got := ts[0]
	want := Template{
		Name: "img1", Image: "img1", SHA256: shaA, Origin: TemplateOriginImage,
		Pool: "thin-test", LV: "vmtpl-000000000001", SizeBytes: 10 * tGiB,
		UsedBytes: 5 * tGiB, Clones: 1, Created: got.Created,
	}
	if got != want || got.Created.IsZero() {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestThinTemplateFromVolumeSaved(t *testing.T) {
	v := ThinVolume{Name: "vmtpl-0123456789ab", VG: "vg-test", Pool: "thin-test", SizeBytes: 20 * tGiB, DataPercent: 25,
		Kind: ThinKindTemplate, Tags: []string{"vmtool_template", "vmtool_name=proj-base", "vmtool_origin=saved"}}
	clones := []ThinVolume{
		{Name: "vm1", VG: "vg-test", Pool: "thin-test", Origin: "vmtpl-0123456789ab", Kind: ThinKindVol},
		{Name: "vm2", VG: "vg-two", Pool: "thin-test", Origin: "vmtpl-0123456789ab", Kind: ThinKindVol}, // other VG
		{Name: "vm3", VG: "vg-test", Pool: "thin-test", Origin: "other", Kind: ThinKindVol},
	}
	tpl, err := TemplateFromVolume(v, append([]ThinVolume{v}, clones...))
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Name != "proj-base" || tpl.Image != "" || tpl.SHA256 != "" || tpl.Origin != TemplateOriginSaved ||
		tpl.Clones != 1 || tpl.UsedBytes != 5*tGiB || tpl.LV != v.Name || tpl.Pool != "thin-test" {
		t.Fatalf("tpl = %+v", tpl)
	}
	// broken tag sets are refused
	for name, tags := range map[string][]string{
		"no name":        {"vmtool_template", "vmtool_origin=saved"},
		"no origin":      {"vmtool_template", "vmtool_name=x"},
		"bad origin":     {"vmtool_template", "vmtool_name=x", "vmtool_origin=cloud"},
		"image no sha":   {"vmtool_template", "vmtool_name=x", "vmtool_origin=image"},
		"bad name value": {"vmtool_template", "vmtool_name=a:b", "vmtool_origin=saved"},
	} {
		bv := v
		bv.Tags = tags
		if _, err := TemplateFromVolume(bv, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	notTpl := v
	notTpl.Kind = ThinKindVol
	if _, err := TemplateFromVolume(notTpl, nil); err == nil {
		t.Error("a plain volume was taken for a template")
	}
}

func tpl(lv, name, image, sha, origin string) Template {
	return Template{LV: lv, Name: name, Image: image, SHA256: sha, Origin: origin, Pool: "thin-test"}
}

func TestThinStaleTemplates(t *testing.T) {
	cur := tpl("vmtpl-aaaaaaaaaaaa", "img1", "img1", shaA, TemplateOriginImage)
	old := tpl("vmtpl-bbbbbbbbbbbb", "img1", "img1", shaB, TemplateOriginImage)
	gone := tpl("vmtpl-cccccccccccc", "img2", "img2", shaA, TemplateOriginImage)
	saved := tpl("vmtpl-dddddddddddd", "img1", "", "", TemplateOriginSaved)
	nosha := tpl("vmtpl-eeeeeeeeeeee", "img1", "img1", "", TemplateOriginImage)
	all := []Template{cur, old, gone, saved, nosha}

	got := StaleTemplates(all, map[string]string{"img1": shaA})
	if !reflect.DeepEqual(got, []Template{old, gone, nosha}) {
		t.Fatalf("stale = %+v", got)
	}
	if got := StaleTemplates(all, nil); len(got) != 4 {
		t.Fatalf("empty manifest: %+v", got)
	}
	if got := StaleTemplates(nil, map[string]string{"img1": shaA}); len(got) != 0 {
		t.Fatalf("no templates: %+v", got)
	}
	if got := StaleTemplates([]Template{saved}, nil); len(got) != 0 {
		t.Fatalf("saved is never stale: %+v", got)
	}
}

func TestThinImageSHAs(t *testing.T) {
	got := ImageSHAs(map[string]ImageEntry{"img1": {SHA256: shaA, Size: 5}, "img2": {SHA256: shaB}})
	if !reflect.DeepEqual(got, map[string]string{"img1": shaA, "img2": shaB}) {
		t.Fatalf("got %v", got)
	}
}

func TestThinFindTemplate(t *testing.T) {
	cur := tpl("vmtpl-aaaaaaaaaaaa", "img1", "img1", shaA, TemplateOriginImage)
	old := tpl("vmtpl-bbbbbbbbbbbb", "img1", "img1", shaB, TemplateOriginImage)
	savedImg1 := tpl("vmtpl-dddddddddddd", "img1", "", "", TemplateOriginSaved)
	savedProj := tpl("vmtpl-eeeeeeeeeeee", "proj", "", "", TemplateOriginSaved)
	all := []Template{old, cur, savedImg1, savedProj}

	for _, c := range []struct {
		name, sha string
		want      string // LV, "" for none
	}{
		{"img1", shaA, cur.LV},
		{"img1", shaB, old.LV},
		{"proj", "", savedProj.LV},
		{"proj", shaA, savedProj.LV},                    // no image template for proj: the saved one
		{"img1", "", savedImg1.LV},                      // no sha: only a saved template can match
		{"img1", strings.Repeat("c", 64), savedImg1.LV}, // never a stale image template for another sha
		{"img2", shaA, ""},
		{"", "", ""},
	} {
		got, ok := FindTemplate(all, c.name, c.sha)
		if (c.want == "") == ok || (ok && got.LV != c.want) {
			t.Errorf("FindTemplate(%q,%.4q) = %q,%v want %q", c.name, c.sha, got.LV, ok, c.want)
		}
	}
	if _, ok := FindTemplate([]Template{old}, "img1", shaA); ok {
		t.Error("returned an image template of another sha")
	}
	if _, ok := FindTemplate([]Template{old}, "img1", ""); ok {
		t.Error("empty sha matched an image template")
	}
}

// thinStub installs a stub sudo that appends one argument per line to a log
// (a "---" line ends each call), prints $STUB_OUT, writes $STUB_ERR to stderr
// and exits $STUB_RC.
func thinStub(t *testing.T) (log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "sudo.log")
	testexec.Stub(t, map[string]string{"sudo": `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> '` + log + `'; done
echo --- >> '` + log + `'
[ -n "$STUB_OUT" ] && printf '%s' "$STUB_OUT"
[ -n "$STUB_ERR" ] && printf '%s\n' "$STUB_ERR" >&2
exit ${STUB_RC:-0}
`})
	return log
}

func readCall(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if lines[len(lines)-1] == "---" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func TestThinClientArgv(t *testing.T) {
	const helper = "/usr/local/libexec/vmtool-thin"
	th := &Thin{}
	for _, c := range []struct {
		name string
		run  func() error
		want []string
	}{
		{"create", func() error {
			return th.Create("vg-test", "thin-test", "vm1", 20*tGiB, "vmtool_vol")
		}, []string{"-n", helper, "create", "vg-test", "thin-test", "vm1", "21474836480", "vmtool_vol"}},
		{"snapshot", func() error {
			return th.Snapshot("vg-test", "vmtpl-000000000001", "vm1", "vmtool_vol")
		}, []string{"-n", helper, "snapshot", "vg-test", "vmtpl-000000000001", "vm1", "vmtool_vol"}},
		{"extend", func() error { return th.Extend("vg-test", "vm1", 30*tGiB) },
			[]string{"-n", helper, "extend", "vg-test", "vm1", "32212254720"}},
		{"remove", func() error { return th.Remove("vg-test", "vm1") },
			[]string{"-n", helper, "remove", "vg-test", "vm1"}},
		{"rename", func() error { return th.Rename("vg-test", "vm1", "vm1-2") },
			[]string{"-n", helper, "rename", "vg-test", "vm1", "vm1-2"}},
		{"tag", func() error { return th.Tag("vg-test", "vm1", "+vmtool_vol", "-vmtool_building") },
			[]string{"-n", helper, "tag", "vg-test", "vm1", "+vmtool_vol", "-vmtool_building"}},
		{"import", func() error { return th.Import("vg-test", "vm1", "/var/lib/vmtool/img1.qcow2") },
			[]string{"-n", helper, "import", "vg-test", "vm1", "/var/lib/vmtool/img1.qcow2"}},
		{"export", func() error { return th.Export("vg-test", "vm1", "/var/lib/vmtool/.img1.qcow2.part") },
			[]string{"-n", helper, "export", "vg-test", "vm1", "/var/lib/vmtool/.img1.qcow2.part"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := thinStub(t)
			if err := c.run(); err != nil {
				t.Fatal(err)
			}
			if got := readCall(t, log); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argv = %q\nwant   %q", got, c.want)
			}
		})
	}
}

func TestThinClientHelperPathField(t *testing.T) {
	log := thinStub(t)
	th := &Thin{Helper: "/opt/stub/vmtool-thin"}
	if err := th.Remove("vg-test", "vm1"); err != nil {
		t.Fatal(err)
	}
	if got := readCall(t, log); got[1] != "/opt/stub/vmtool-thin" {
		t.Fatalf("argv = %q", got)
	}
}

func TestThinClientRejectsBeforeRunning(t *testing.T) {
	log := thinStub(t)
	th := &Thin{}
	for name, err := range map[string]error{
		"zero bytes":     th.Create("vg-test", "thin-test", "vm1", 0, "vmtool_vol"),
		"negative bytes": th.Extend("vg-test", "vm1", -5),
		"bad tag":        th.Create("vg-test", "thin-test", "vm1", tGiB, "bad tag"),
		"bad tag change": th.Tag("vg-test", "vm1", "vmtool_vol"),
		"no tag change":  th.Tag("vg-test", "vm1"),
		"flag as name":   th.Remove("-y", "vm1"),
		"empty name":     th.Remove("vg-test", ""),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := os.Stat(log); err == nil {
		t.Errorf("the helper was run: %q", readCall(t, log))
	}
}

func TestThinClientReport(t *testing.T) {
	log := thinStub(t)
	t.Setenv("STUB_OUT", fixtureReport())
	pools, vols, err := (&Thin{}).Report()
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || len(vols) != 4 {
		t.Fatalf("pools=%+v vols=%d", pools, len(vols))
	}
	if got := readCall(t, log); !reflect.DeepEqual(got, []string{"-n", "/usr/local/libexec/vmtool-thin", "report"}) {
		t.Fatalf("argv = %q", got)
	}
}

func TestThinClientReportEmptyAndMalformed(t *testing.T) {
	thinStub(t)
	t.Setenv("STUB_OUT", `{"report": [{"lv": []}]}`)
	pools, vols, err := (&Thin{}).Report()
	if err != nil || len(pools) != 0 || len(vols) != 0 {
		t.Fatalf("empty: %v %v %v", pools, vols, err)
	}
	t.Setenv("STUB_OUT", `{"report": [`)
	_, _, err = (&Thin{}).Report()
	if err == nil || !strings.Contains(err.Error(), "report") {
		t.Fatalf("malformed: %v", err)
	}
}

func TestThinClientRefusal(t *testing.T) {
	thinStub(t)
	t.Setenv("STUB_RC", "1")
	t.Setenv("STUB_ERR", "vmtool-thin: vm1 is not a vmtool volume")
	err := (&Thin{}).Remove("vg-test", "vm1")
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), "vm1 is not a vmtool volume") || !strings.Contains(err.Error(), "remove") {
		t.Fatalf("error = %q", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("error spans lines: %q", err)
	}
	// report refusal too
	if _, _, err := (&Thin{}).Report(); err == nil || !strings.Contains(err.Error(), "not a vmtool volume") {
		t.Fatalf("report refusal = %v", err)
	}
	// sudo itself refusing (no rule) comes through the same way
	t.Setenv("STUB_ERR", "sudo: a password is required")
	err = (&Thin{}).Remove("vg-test", "vm1")
	if err == nil || !strings.Contains(err.Error(), "a password is required") {
		t.Fatalf("sudo refusal = %v", err)
	}
}

func TestThinClientRefusalWithoutStderr(t *testing.T) {
	thinStub(t)
	t.Setenv("STUB_RC", "3")
	err := (&Thin{}).Remove("vg-test", "vm1")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("error = %v", err)
	}
}

func TestThinClientRefusesRealSudoUnderTest(t *testing.T) {
	// no stub installed: command() must refuse before anything starts
	t.Setenv(testStubsEnv, "")
	if err := (&Thin{}).Remove("vg-test", "vm1"); err == nil {
		t.Fatal("ran without a stub")
	}
}
