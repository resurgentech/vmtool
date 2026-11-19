package hosts

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/testexec"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const (
	sha64a    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha64b    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tplLV     = "vmtpl-aaaaaaaaaaaa" // TemplateLVName(sha64a)
	bldLV     = "vmtbld-aaaaaaaaaaaa"
	savedLV   = "vmtpl-000000000001"
	gib       = int64(1 << 30)
	tplTagsA  = "vmtool_template,vmtool_name=img1,vmtool_sha256=" + sha64a + ",vmtool_origin=image"
	savedTags = "vmtool_template,vmtool_name=proj,vmtool_origin=saved"
)

// lvRow is one lvs JSON row.
func lvRow(name, attr string, size int64, pool, tags string) string {
	return fmt.Sprintf(`{"lv_name":%q,"vg_name":"vg-test","lv_attr":%q,"lv_size":"%d","pool_lv":%q,"origin":"","data_percent":"","metadata_percent":"","lv_tags":%q,"lv_time":""}`,
		name, attr, size, pool, tags)
}

// poolRow is the 60 GiB pool thin-test at data percent data.
func poolRow(data string) string {
	return fmt.Sprintf(`{"lv_name":"thin-test","vg_name":"vg-test","lv_attr":"twi-aotz--","lv_size":"%d","pool_lv":"","origin":"","data_percent":%q,"metadata_percent":"1.00","lv_tags":"vmtool_thinpool","lv_time":""}`, 60*gib, data)
}

func reportOf(rows ...string) string {
	return `{"report":[{"lv":[` + strings.Join(rows, ",") + `]}]}`
}

func thinLV(name string, size int64, tags string) string {
	return lvRow(name, "Vwi-aotz--", size, "thin-test", tags)
}

// helperStub is a stub sudo standing in for the helper: it logs "<verb> <args>"
// per call into log, answers `report` with report.json, fails a verb when
// fail_<verb> exists (its content is the stderr line), runs on_<verb> (a shell
// snippet) after a verb, and holds `import` until release exists when
// block_import does (at most 10 s).
type helperStub struct {
	dir string
	log string
}

func newHelperStub(t *testing.T) *helperStub {
	t.Helper()
	dir := t.TempDir()
	h := &helperStub{dir: dir, log: filepath.Join(dir, "log")}
	script := `#!/bin/sh
ST='` + dir + `'
shift 2
verb=$1
shift
if [ $# -gt 0 ]; then echo "$verb $*" >> "$ST/log"; else echo "$verb" >> "$ST/log"; fi
if [ -f "$ST/fail_$verb" ]; then cat "$ST/fail_$verb" >&2; exit 1; fi
if [ "$verb" = import ] && [ -f "$ST/block_import" ]; then
  i=0
  while [ ! -f "$ST/release" ] && [ "$i" -lt 100 ]; do sleep 0.1; i=$((i+1)); done
fi
if [ -f "$ST/on_$verb" ]; then . "$ST/on_$verb"; fi
if [ "$verb" = report ]; then cat "$ST/report.json"; fi
exit 0
`
	testexec.Stub(t, map[string]string{"sudo": script})
	return h
}

func (h *helperStub) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *helperStub) report(t *testing.T, rep string) { h.write(t, "report.json", rep) }

// calls is every helper call so far, in order, "verb args".
func (h *helperStub) calls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(h.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (h *helperStub) count(t *testing.T, verb string) int {
	n := 0
	for _, c := range h.calls(t) {
		if c == verb || strings.HasPrefix(c, verb+" ") {
			n++
		}
	}
	return n
}

func (h *helperStub) writes(t *testing.T) []string {
	var out []string
	for _, c := range h.calls(t) {
		if c != "report" {
			out = append(out, c)
		}
	}
	return out
}

// cache is an image cache directory with the file img1 (a qcow2 header of the
// given virtual size) and a manifest listing it with the given SHA-256.
type cache struct {
	dir, manifest, img string
}

func newCache(t *testing.T, virtual uint64, sha string) cache {
	t.Helper()
	dir := t.TempDir()
	b := make([]byte, 72)
	copy(b, []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint64(b[24:], virtual)
	img := filepath.Join(dir, "img1")
	if err := os.WriteFile(img, b, 0o644); err != nil {
		t.Fatal(err)
	}
	man := filepath.Join(t.TempDir(), "image-manifest.yml")
	if err := os.WriteFile(man, []byte(fmt.Sprintf("img1:\n  sha256: %s\n  size: %d\n", sha, len(b))), 0o644); err != nil {
		t.Fatal(err)
	}
	return cache{dir: dir, manifest: man, img: img}
}

// thinInner is the fake libvirt Manager under ThinPools: one dir pool
// "vmtool", every call recorded into events (a shared slice, so a test sees
// the order against the helper's log through the marker file).
type thinInner struct {
	app.Manager
	mu        sync.Mutex
	events    []string
	pools     []vmtool.PoolInfo
	disks     []string
	disksErr  error
	deleteErr error
	logFile   string // when set, Delete also appends "INNER Delete" here
	defined   vmtool.VMConfig
}

func (f *thinInner) rec(format string, a ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, fmt.Sprintf(format, a...))
}

func (f *thinInner) ListPools() ([]vmtool.PoolInfo, error) {
	return append([]vmtool.PoolInfo{{Name: "vmtool", Path: "/var/lib/vmtool/images", Active: true, Type: "dir"}}, f.pools...), nil
}
func (f *thinInner) CloneImage(base, name, pool string, size uint64) (string, error) {
	f.rec("CloneImage %s %s %s %d", base, name, pool, size)
	return "/var/lib/vmtool/images/" + name + ".qcow2", nil
}
func (f *thinInner) Define(c vmtool.VMConfig) error {
	f.rec("Define %s", c.Name)
	f.defined = c
	return nil
}
func (f *thinInner) Delete(name string, noclone bool) error {
	f.rec("Delete %s %v", name, noclone)
	if f.logFile != "" {
		fh, err := os.OpenFile(f.logFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err == nil {
			fmt.Fprintln(fh, "INNER Delete")
			fh.Close()
		}
	}
	return f.deleteErr
}
func (f *thinInner) BlockDiskPaths(name string) ([]string, error) {
	f.rec("BlockDiskPaths %s", name)
	return f.disks, f.disksErr
}
func (f *thinInner) ResizeVolume(p string, b uint64) error {
	f.rec("ResizeVolume %s %d", p, b)
	return nil
}
func (f *thinInner) CreateVolume(pool, name string, gb uint) (string, error) {
	f.rec("CreateVolume %s %s %d", pool, name, gb)
	return "/var/lib/vmtool/images/" + name, nil
}
func (f *thinInner) ImagePath(name string) (string, error) {
	f.rec("ImagePath %s", name)
	return "", fmt.Errorf("image %q not found in any storage pool: %w", name, vmtool.ErrNotFound)
}

// lastDefined is the last Define's config.
func (f *thinInner) lastDefined() vmtool.VMConfig { return f.defined }

func newThinFor(t *testing.T, inner app.Manager, c cache) ThinPools {
	t.Helper()
	return ThinPools{Manager: inner, Thin: &vmtool.Thin{Helper: thinHelper}, Log: logger(&bytes.Buffer{}),
		Builds: &ThinBuilds{}, ManifestPath: c.manifest}
}

const gb20 = uint64(20 << 30)

// warmReport: the pool at 25 % holding the image template of img1 (10 GiB).
func warmReport() string {
	return reportOf(poolRow("25.00"), thinLV(tplLV, 10*gib, tplTagsA))
}

func argvWarm20() []string {
	return []string{
		"report",
		"snapshot vg-test " + tplLV + " vm1 vmtool_vol",
		"extend vg-test vm1 21474836480",
	}
}

func eqStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestThinCloneWarm(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, warmReport())
	c := newCache(t, uint64(10*gib), sha64a)
	inner := &thinInner{}
	m := newThinFor(t, inner, c)

	path, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20)
	if err != nil || path != "/dev/vg-test/vm1" {
		t.Fatalf("CloneImageFrom = %q, %v", path, err)
	}
	eqStrings(t, "helper argv", h.calls(t), argvWarm20())
	for _, e := range inner.events {
		if strings.HasPrefix(e, "CloneImage") {
			t.Errorf("the libvirt Manager cloned: %q", e)
		}
	}

	// CloneImage (an older client: no image name) derives it from base_path.
	h2 := newHelperStub(t)
	h2.report(t, warmReport())
	path, err = m.CloneImage(c.img, "vm1", "thin-test", gb20)
	if err != nil || path != "/dev/vg-test/vm1" {
		t.Fatalf("CloneImage = %q, %v", path, err)
	}
	eqStrings(t, "helper argv (derived image name)", h2.calls(t), argvWarm20())

	// The request's name may differ from the cached file's by lookup alone
	// ("img1" found as "img1.qcow2"): the cached file's name wins (FOREMAN).
	h3 := newHelperStub(t)
	h3.report(t, warmReport())
	path, err = m.CloneImageFrom("img1-as-asked", c.img, "vm1", "thin-test", gb20)
	if err != nil || path != "/dev/vg-test/vm1" {
		t.Fatalf("CloneImageFrom with another name = %q, %v", path, err)
	}
	eqStrings(t, "helper argv (cache file's name)", h3.calls(t), argvWarm20())
}

func TestThinCloneWarmNoExtendWhenTemplateIsBigEnough(t *testing.T) {
	for name, size := range map[string]uint64{"zero": 0, "smaller": uint64(5 * gib), "equal": uint64(10 * gib)} {
		h := newHelperStub(t)
		h.report(t, warmReport())
		c := newCache(t, uint64(10*gib), sha64a)
		m := newThinFor(t, &thinInner{}, c)
		if _, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", size); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		eqStrings(t, name+": helper argv", h.calls(t), []string{"report", "snapshot vg-test " + tplLV + " vm1 vmtool_vol"})
	}
}

// The cold path: nothing in the pool yet. After rename, the report is
// re-read, because lvcreate rounds the template up to whole extents and the
// extend decision must use the real size.
func TestThinCloneCold(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00")))
	h.write(t, "report_after.json", warmReport())
	h.write(t, "on_rename", `cp "$ST/report_after.json" "$ST/report.json"`)
	// 10 GiB less 1 MiB: the LV is rounded up to exactly 10 GiB, so asking
	// for 10 GiB must not extend.
	virtual := uint64(10*gib - 1<<20)
	c := newCache(t, virtual, sha64a)
	m := newThinFor(t, &thinInner{}, c)

	path, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", uint64(10*gib))
	if err != nil || path != "/dev/vg-test/vm1" {
		t.Fatalf("CloneImageFrom = %q, %v", path, err)
	}
	want := []string{
		"report",
		"report",
		fmt.Sprintf("create vg-test thin-test %s %d vmtool_building", bldLV, virtual),
		"import vg-test " + bldLV + " " + c.img,
		"tag vg-test " + bldLV + " +vmtool_template +vmtool_name=img1 +vmtool_sha256=" + sha64a + " +vmtool_origin=image -vmtool_building",
		"rename vg-test " + bldLV + " " + tplLV,
		"report",
		"snapshot vg-test " + tplLV + " vm1 vmtool_vol",
	}
	eqStrings(t, "helper argv", h.calls(t), want)

	// and with a larger request, the extend follows the snapshot
	h = newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00")))
	h.write(t, "report_after.json", warmReport())
	h.write(t, "on_rename", `cp "$ST/report_after.json" "$ST/report.json"`)
	m = newThinFor(t, &thinInner{}, c)
	if _, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20); err != nil {
		t.Fatal(err)
	}
	got := h.calls(t)
	if len(got) != len(want)+1 || got[len(got)-1] != "extend vg-test vm1 21474836480" || got[len(got)-2] != want[len(want)-1] {
		t.Errorf("cold argv with extend: %q", got)
	}
}

func TestThinCloneNoRoomWritesNothing(t *testing.T) {
	// warm: data use at 80 %
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("80.00"), thinLV(tplLV, 10*gib, tplTagsA)))
	c := newCache(t, uint64(10*gib), sha64a)
	m := newThinFor(t, &thinInner{}, c)
	_, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20)
	if !errors.Is(err, vmtool.ErrThinNoRoom) {
		t.Fatalf("err = %v, want ErrThinNoRoom", err)
	}
	var ae *app.Error
	if !app.AsError(err, &ae) || ae.Kind != app.KindConflict {
		t.Errorf("error is not a conflict: %v", err)
	}
	if !strings.Contains(err.Error(), "80.00%") {
		t.Errorf("error lacks the numbers: %v", err)
	}
	eqStrings(t, "warm helper argv", h.calls(t), []string{"report"})

	// cold: the pool would be promised past 4x (template + clone)
	h = newHelperStub(t)
	h.report(t, reportOf(poolRow("10.00"), thinLV("other", 215*gib, "")))
	m = newThinFor(t, &thinInner{}, c)
	_, err = m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20)
	if !errors.Is(err, vmtool.ErrThinNoRoom) {
		t.Fatalf("cold err = %v, want ErrThinNoRoom", err)
	}
	// 215 + 10 (template) + 20 (clone) = 245 > 240
	eqStrings(t, "cold helper argv", h.calls(t), []string{"report", "report"})
	if !strings.Contains(err.Error(), "245.0 GiB") {
		t.Errorf("cold refusal does not promise template + clone: %v", err)
	}

	// 205 + 10 + 20 = 235 <= 240: room, and the build goes ahead
	h = newHelperStub(t)
	h.report(t, reportOf(poolRow("10.00"), thinLV("other", 205*gib, "")))
	h.write(t, "report_after.json", reportOf(poolRow("10.00"), thinLV("other", 205*gib, ""), thinLV(tplLV, 10*gib, tplTagsA)))
	h.write(t, "on_rename", `cp "$ST/report_after.json" "$ST/report.json"`)
	m = newThinFor(t, &thinInner{}, c)
	if _, err = m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20); err != nil {
		t.Fatalf("with room: %v", err)
	}
}

// Two clones of one image at once build once: the second waits for the lock,
// re-reads the report and finds the finished template.
func TestThinCloneConcurrentBuildsOnce(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00")))
	h.write(t, "report_after.json", warmReport())
	h.write(t, "on_rename", `cp "$ST/report_after.json" "$ST/report.json"`)
	h.write(t, "block_import", "")
	c := newCache(t, uint64(10*gib), sha64a)
	builds := &ThinBuilds{}
	mk := func() ThinPools {
		m := newThinFor(t, &thinInner{}, c)
		m.Builds = builds // one lock table for the whole process
		return m
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	run := func(i int, name string) {
		defer wg.Done()
		_, errs[i] = mk().CloneImageFrom("img1", c.img, name, "thin-test", gb20)
	}
	wg.Add(1)
	go run(0, "vm1")
	// wait until the first clone is inside import
	waitFor(t, func() bool { return h.count(t, "import") == 1 })
	wg.Add(1)
	go run(1, "vm2")
	// the second one has read its report and is now waiting for the lock
	waitFor(t, func() bool { return h.count(t, "report") == 3 })
	time.Sleep(200 * time.Millisecond)
	if n := h.count(t, "create"); n != 1 {
		t.Errorf("creates while the first build runs: %d", n)
	}
	h.write(t, "release", "")
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("clone %d: %v", i, err)
		}
	}
	for _, verb := range []string{"create", "import", "tag", "rename"} {
		if n := h.count(t, verb); n != 1 {
			t.Errorf("%s ran %d times, want 1: %q", verb, n, h.calls(t))
		}
	}
	calls := strings.Join(h.calls(t), "\n")
	for _, vm := range []string{"vm1", "vm2"} {
		if !strings.Contains(calls, "snapshot vg-test "+tplLV+" "+vm+" vmtool_vol") {
			t.Errorf("no snapshot for %s:\n%s", vm, calls)
		}
	}
	if n := h.count(t, "remove"); n != 0 {
		t.Errorf("remove ran %d times", n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the helper stub")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func coldFixture(t *testing.T) (*helperStub, cache, ThinPools) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00")))
	h.write(t, "report_after.json", warmReport())
	h.write(t, "on_rename", `cp "$ST/report_after.json" "$ST/report.json"`)
	c := newCache(t, uint64(10*gib), sha64a)
	return h, c, newThinFor(t, &thinInner{}, c)
}

func TestThinBuildFailureRemovesTheBuildingLV(t *testing.T) {
	for _, verb := range []string{"import", "tag", "rename"} {
		h, c, m := coldFixture(t)
		h.write(t, "fail_"+verb, "vmtool-thin: "+verb+" went wrong\n")
		_, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20)
		if err == nil || !strings.Contains(err.Error(), verb+" went wrong") {
			t.Fatalf("%s: err = %v, want the original error", verb, err)
		}
		var removes []string
		for _, w := range h.writes(t) {
			if strings.HasPrefix(w, "remove ") {
				removes = append(removes, w)
			}
			if strings.HasPrefix(w, "snapshot ") {
				t.Errorf("%s: snapshot after a failed build", verb)
			}
		}
		eqStrings(t, verb+": removes", removes, []string{"remove vg-test " + bldLV})
	}

	// create itself failing leaves nothing to remove
	h, c, m := coldFixture(t)
	h.write(t, "fail_create", "vmtool-thin: create refused\n")
	if _, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20); err == nil {
		t.Fatal("want an error")
	}
	if n := h.count(t, "remove"); n != 0 {
		t.Errorf("remove after a failed create: %q", h.calls(t))
	}

	// a failed removal is named in the error, with the original
	h, c, m = coldFixture(t)
	h.write(t, "fail_import", "vmtool-thin: import went wrong\n")
	h.write(t, "fail_remove", "vmtool-thin: remove refused\n")
	_, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20)
	if err == nil || !strings.Contains(err.Error(), "import went wrong") || !strings.Contains(err.Error(), bldLV) || !strings.Contains(err.Error(), "remove refused") {
		t.Errorf("err = %v", err)
	}
}

// After a failed build the lock is free: the next clone builds again.
func TestThinBuildFailureReleasesTheLock(t *testing.T) {
	h, c, m := coldFixture(t)
	h.write(t, "fail_import", "vmtool-thin: import went wrong\n")
	if _, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20); err == nil {
		t.Fatal("want an error")
	}
	if err := os.Remove(filepath.Join(h.dir, "fail_import")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20); err != nil {
		t.Fatalf("second clone: %v", err)
	}
}

func TestThinExtendFailureRemovesTheNewLV(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, warmReport())
	h.write(t, "fail_extend", "vmtool-thin: extend refused\n")
	c := newCache(t, uint64(10*gib), sha64a)
	m := newThinFor(t, &thinInner{}, c)
	_, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20)
	if err == nil || !strings.Contains(err.Error(), "extend refused") {
		t.Fatalf("err = %v", err)
	}
	eqStrings(t, "helper argv", h.calls(t), append(argvWarm20(), "remove vg-test vm1"))
}

func TestThinSnapshotFailureRemovesNothing(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, warmReport())
	h.write(t, "fail_snapshot", "vmtool-thin: vm1 already exists\n")
	c := newCache(t, uint64(10*gib), sha64a)
	m := newThinFor(t, &thinInner{}, c)
	_, err := m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
	eqStrings(t, "helper argv", h.calls(t), argvWarm20()[:2])
}

func TestThinCloneRefusals(t *testing.T) {
	c := newCache(t, uint64(10*gib), sha64a)
	rawFile := newCache(t, uint64(10*gib), sha64a)
	if err := os.WriteFile(rawFile.img, bytes.Repeat([]byte{0}, 72), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := reportOf(poolRow("25.00"))
	for _, tc := range []struct {
		name      string
		cache     cache
		image     string
		base, vm  string
		wantCalls []string
		wantMsg   string
	}{
		{"vmtpl- name", c, "img1", c.img, "vmtpl-x", []string{"report"}, "vmtpl-"},
		{"image not in manifest", c, "", filepath.Join(c.dir, "other"), "vm1", []string{"report"}, "manifest"},
		{"not a qcow2: refused before any write", rawFile, "img1", rawFile.img, "vm1", []string{"report", "report"}, "qcow2"},
	} {
		h := newHelperStub(t)
		h.report(t, empty)
		m := newThinFor(t, &thinInner{}, tc.cache)
		_, err := m.CloneImageFrom(tc.image, tc.base, tc.vm, "thin-test", gb20)
		var ae *app.Error
		if err == nil || !app.AsError(err, &ae) || ae.Kind != app.KindInvalid || !strings.Contains(err.Error(), tc.wantMsg) {
			t.Errorf("%s: err = %v, want an invalid error containing %q", tc.name, err, tc.wantMsg)
		}
		eqStrings(t, tc.name+": helper argv", h.calls(t), tc.wantCalls)
	}
}

// A non-thin target never reaches the helper: the call is the inner
// Manager's, unchanged.
func TestThinCloneNonThinPassesThrough(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, warmReport())
	c := newCache(t, uint64(10*gib), sha64a)
	inner := &thinInner{}
	m := newThinFor(t, inner, c)
	path, err := m.CloneImageFrom("img1", "/var/lib/vmtool/images/img1", "vm1", "vmtool", gb20)
	if err != nil || path != "/var/lib/vmtool/images/vm1.qcow2" {
		t.Fatalf("clone = %q, %v", path, err)
	}
	if got := h.calls(t); len(got) != 0 {
		t.Errorf("helper ran for a libvirt pool: %q", got)
	}
	want := fmt.Sprintf("CloneImage /var/lib/vmtool/images/img1 vm1 vmtool %d", gb20)
	if got := inner.events[len(inner.events)-1]; got != want {
		t.Errorf("inner call = %q, want %q", got, want)
	}
	// a libvirt pool of the same name as a thin pool wins (the thin pool is
	// not listed, as in ListPools)
	inner2 := &thinInner{pools: []vmtool.PoolInfo{{Name: "thin-test", Path: "/srv/x", Type: "dir", Active: true}}}
	m2 := newThinFor(t, inner2, c)
	if _, err := m2.CloneImageFrom("img1", "/srv/x/img1", "vm1", "thin-test", 0); err != nil {
		t.Fatal(err)
	}
	if got := h.calls(t); len(got) != 0 {
		t.Errorf("helper ran for a libvirt pool: %q", got)
	}
}

// A saved template answers when the cache lacks the image; and a cached image
// is never shadowed by a saved template of the same name or by an image
// template of another SHA-256.
func TestThinCloneTemplateSelection(t *testing.T) {
	// base_path is a saved template's LV (the template-only case)
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV(savedLV, 10*gib, savedTags)))
	c := newCache(t, uint64(10*gib), sha64a)
	m := newThinFor(t, &thinInner{}, c)
	path, err := m.CloneImageFrom("proj", "/dev/vg-test/"+savedLV, "vm1", "thin-test", gb20)
	if err != nil || path != "/dev/vg-test/vm1" {
		t.Fatalf("clone from saved = %q, %v", path, err)
	}
	eqStrings(t, "saved: helper argv", h.calls(t), []string{
		"report", "snapshot vg-test " + savedLV + " vm1 vmtool_vol", "extend vg-test vm1 21474836480"})

	// the template is in another pool than the target
	h = newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV(savedLV, 10*gib, savedTags),
		strings.Replace(poolRow("1.00"), "thin-test", "other-thin", 1)))
	m = newThinFor(t, &thinInner{}, c)
	if _, err := m.CloneImageFrom("proj", "/dev/vg-test/"+savedLV, "vm1", "other-thin", gb20); err == nil {
		t.Error("a template of another pool was cloned")
	}
	if n := len(h.writes(t)); n != 0 {
		t.Errorf("writes: %q", h.writes(t))
	}

	// a vol LV (someone's disk) is not a template to clone
	h = newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("web", 10*gib, "vmtool_vol")))
	m = newThinFor(t, &thinInner{}, c)
	if _, err := m.CloneImageFrom("web", "/dev/vg-test/web", "vm1", "thin-test", gb20); err == nil {
		t.Error("a VM disk was cloned as a template")
	}
	if n := len(h.writes(t)); n != 0 {
		t.Errorf("writes: %q", h.writes(t))
	}

	// the cache has img1; the pool has only a saved template named img1: the
	// image is built, the saved template is not used
	h = newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV(savedLV, 10*gib, "vmtool_template,vmtool_name=img1,vmtool_origin=saved")))
	m = newThinFor(t, &thinInner{}, c)
	_, _ = m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20)
	if h.count(t, "create") != 1 || strings.Contains(strings.Join(h.calls(t), "\n"), "snapshot vg-test "+savedLV) {
		t.Errorf("a saved template shadowed the cached image: %q", h.calls(t))
	}

	// an image template of an older SHA-256 is not used either
	h = newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vmtpl-bbbbbbbbbbbb", 10*gib,
		"vmtool_template,vmtool_name=img1,vmtool_sha256="+sha64b+",vmtool_origin=image")))
	m = newThinFor(t, &thinInner{}, c)
	_, _ = m.CloneImageFrom("img1", c.img, "vm1", "thin-test", gb20)
	if h.count(t, "create") != 1 || strings.Contains(strings.Join(h.calls(t), "\n"), "snapshot vg-test vmtpl-bbbbbbbbbbbb") {
		t.Errorf("a stale template was used: %q", h.calls(t))
	}
}

// ---- Define ----

func TestThinDefineSetsBlockKind(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_vol"), thinLV("vm1-2", 10*gib, "vmtool_vol"),
		thinLV("foreign", gib, "")))
	inner := &thinInner{}
	m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	cfg := vmtool.VMConfig{Name: "vm1", DiskPath: "/dev/vg-test/vm1", Pool: "thin-test", DiskCache: "none",
		ExtraDisks: []vmtool.DiskDevice{
			{Path: "/dev/vg-test/vm1-2", Target: "vdb"},
			{Path: "/var/lib/vmtool/images/x.qcow2", Target: "vdc"},
			{Path: "/dev/vg-test/foreign", Target: "vdd"},
		}}
	if err := m.Define(cfg); err != nil {
		t.Fatal(err)
	}
	got := inner.lastDefined()
	if got.DiskKind != vmtool.DiskBackingBlock {
		t.Errorf("vda kind = %q", got.DiskKind)
	}
	var kinds []string
	for _, d := range got.ExtraDisks {
		kinds = append(kinds, d.Kind)
	}
	eqStrings(t, "extra kinds", kinds, []string{vmtool.DiskBackingBlock, "", ""})
	if got.DiskCache != "none" {
		t.Errorf("cache = %q", got.DiskCache)
	}
	if n := h.count(t, "report"); n != 1 {
		t.Errorf("report ran %d times for one Define", n)
	}
	// the caller's slice is not written through
	if cfg.ExtraDisks[0].Kind != "" || cfg.DiskKind != "" {
		t.Error("Define changed the caller's config")
	}
}

func TestThinDefineRefusesTemplatesAndBuilding(t *testing.T) {
	for _, lv := range []string{tplLV, bldLV} {
		h := newHelperStub(t)
		h.report(t, reportOf(poolRow("25.00"), thinLV(tplLV, 10*gib, tplTagsA), thinLV(bldLV, 10*gib, "vmtool_building")))
		inner := &thinInner{}
		m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
		for name, cfg := range map[string]vmtool.VMConfig{
			"boot":  {Name: "vm1", DiskPath: "/dev/vg-test/" + lv},
			"extra": {Name: "vm1", DiskPath: "/dev/vg-test/vm1", ExtraDisks: []vmtool.DiskDevice{{Path: "/dev/vg-test/" + lv, Target: "vdb"}}},
		} {
			err := m.Define(cfg)
			var ae *app.Error
			if err == nil || !app.AsError(err, &ae) || ae.Kind != app.KindInvalid {
				t.Errorf("%s %s: err = %v, want invalid", lv, name, err)
			}
		}
		for _, e := range inner.events {
			if strings.HasPrefix(e, "Define") {
				t.Errorf("%s: libvirt Define reached", lv)
			}
		}
		_ = h
	}
}

// With no /dev/<vg>/<lv> path the helper is not asked; with a helper that
// fails Define goes on as before (the disks stay unresolved).
func TestThinDefineWithoutThinPathsOrHelper(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, warmReport())
	inner := &thinInner{}
	m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	if err := m.Define(vmtool.VMConfig{Name: "web", DiskPath: "/var/lib/vmtool/images/web.qcow2"}); err != nil {
		t.Fatal(err)
	}
	if got := h.calls(t); len(got) != 0 {
		t.Errorf("helper ran: %q", got)
	}
	h.write(t, "fail_report", "sudo: a password is required\n")
	if err := m.Define(vmtool.VMConfig{Name: "web", DiskPath: "/dev/vg/lv"}); err != nil {
		t.Fatal(err)
	}
	if k := inner.lastDefined().DiskKind; k != "" {
		t.Errorf("kind = %q with no report", k)
	}
}

// ---- Delete ----

func TestThinDeleteRemovesVolAfterInnerDelete(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"),
		thinLV("vm1", 20*gib, "vmtool_vol"), thinLV("vm1-2", 10*gib, "vmtool_vol"),
		thinLV("foreignvol", gib, ""), thinLV(tplLV, 10*gib, tplTagsA), thinLV(bldLV, gib, "vmtool_building"),
		thinLV("other", gib, "vmtool_vol"),
		// named for the VM, but not vmtool_vol: a template, a building volume,
		// an untagged thin LV and a thick LV are all left
		thinLV("vm1-3", gib, savedTags), thinLV("vm1-4", gib, "vmtool_building"), thinLV("vm1-5", gib, ""),
		lvRow("vm1-6", "-wi-ao----", gib, "", ""),
		lvRow("thick", "-wi-ao----", gib, "", "")))
	inner := &thinInner{logFile: h.log, disks: []string{
		"/dev/vg-test/vm1", "/dev/vg-test/vm1-2", "/dev/vg-test/foreignvol", "/dev/vg-test/" + tplLV,
		"/dev/vg-test/" + bldLV, "/dev/vg-test/thick", "/dev/vg-test/other", "/dev/vg-test/missing", "/dev/other/vm1",
		"/dev/vg-test/vm1-3", "/dev/vg-test/vm1-4", "/dev/vg-test/vm1-5", "/dev/vg-test/vm1-6"}}
	m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	if err := m.Delete("vm1", false); err != nil {
		t.Fatal(err)
	}
	// the inner Delete ran first; then one report; then the two removals.
	// "other" is a vmtool_vol LV, but not named vm1 or vm1-N: a VM defined on
	// another VM's disk does not take it with it.
	eqStrings(t, "order", h.calls(t), []string{
		"INNER Delete", "report", "remove vg-test vm1", "remove vg-test vm1-2"})
	if inner.events[0] != "BlockDiskPaths vm1" || inner.events[1] != "Delete vm1 false" {
		t.Errorf("inner events = %q: paths must be read before the undefine", inner.events)
	}
}

func TestThinDeleteNoclonePassesThrough(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_vol")))
	inner := &thinInner{logFile: h.log, disks: []string{"/dev/vg-test/vm1"}}
	m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	if err := m.Delete("vm1", true); err != nil {
		t.Fatal(err)
	}
	if got := h.calls(t); len(got) != 1 || got[0] != "INNER Delete" {
		t.Errorf("helper calls = %q", got)
	}
	if len(inner.events) != 1 || inner.events[0] != "Delete vm1 true" {
		t.Errorf("inner events = %q", inner.events)
	}
}

func TestThinDeleteFailures(t *testing.T) {
	// the inner Delete fails: nothing is removed
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_vol")))
	inner := &thinInner{logFile: h.log, disks: []string{"/dev/vg-test/vm1"}, deleteErr: errors.New("undefining domain: boom")}
	m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	if err := m.Delete("vm1", false); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	eqStrings(t, "calls", h.calls(t), []string{"INNER Delete"})

	// the paths cannot be read: the inner Delete runs, its error returns
	h = newHelperStub(t)
	inner = &thinInner{logFile: h.log, disksErr: fmt.Errorf("looking up domain: %w", vmtool.ErrNotFound)}
	m = newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	if err := m.Delete("vm1", false); err != nil {
		t.Fatal(err) // the fake inner Delete succeeds; nothing to remove
	}
	eqStrings(t, "calls", h.calls(t), []string{"INNER Delete"})

	// the report cannot be read: the delete stands, nothing is removed, one
	// warning names the volumes
	h = newHelperStub(t)
	h.write(t, "fail_report", "sudo: a password is required\n")
	var logbuf bytes.Buffer
	inner = &thinInner{logFile: h.log, disks: []string{"/dev/vg-test/vm1"}}
	m = newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	m.Log = logger(&logbuf)
	if err := m.Delete("vm1", false); err != nil {
		t.Fatalf("delete with a broken helper: %v", err)
	}
	if h.count(t, "remove") != 0 || !strings.Contains(logbuf.String(), "vg-test/vm1") {
		t.Errorf("calls %q, log %q", h.calls(t), logbuf.String())
	}

	// a removal fails: the error names the LV and says the domain is gone
	h = newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_vol"), thinLV("vm1-2", 10*gib, "vmtool_vol")))
	h.write(t, "fail_remove", "vmtool-thin: remove refused\n")
	inner = &thinInner{logFile: h.log, disks: []string{"/dev/vg-test/vm1", "/dev/vg-test/vm1-2"}}
	m = newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	err := m.Delete("vm1", false)
	if err == nil || !strings.Contains(err.Error(), "vm1") || !strings.Contains(err.Error(), "already") || !strings.Contains(err.Error(), "remove refused") {
		t.Errorf("err = %v", err)
	}
	// it still tries the other disk
	if n := h.count(t, "remove"); n != 2 {
		t.Errorf("remove calls = %d, want both tried", n)
	}
}

// ---- Resize and CreateVolume ----

func TestThinResizeVolume(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_vol"), thinLV(tplLV, 10*gib, tplTagsA)))
	inner := &thinInner{}
	m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	if err := m.ResizeVolume("/dev/vg-test/vm1", uint64(20*gib)); err != nil {
		t.Errorf("equal size: %v", err)
	}
	if err := m.ResizeVolume("/dev/vg-test/vm1", uint64(10*gib)); err != nil {
		t.Errorf("smaller size: %v", err)
	}
	if n := len(h.writes(t)); n != 0 {
		t.Errorf("writes: %q", h.writes(t))
	}
	// growing one extends it (T6)
	if err := m.ResizeVolume("/dev/vg-test/vm1", uint64(30*gib)); err != nil {
		t.Errorf("growing err = %v", err)
	}
	if err := m.ResizeVolume("/dev/vg-test/"+tplLV, uint64(10*gib)); err == nil {
		t.Error("a template was resized")
	}
	eqStrings(t, "writes", h.writes(t), []string{"extend vg-test vm1 32212254720"})
	// anything else is the inner Manager's
	if err := m.ResizeVolume("/var/lib/vmtool/images/a.qcow2", 5); err != nil {
		t.Fatal(err)
	}
	if got := inner.events; len(got) != 1 || got[0] != "ResizeVolume /var/lib/vmtool/images/a.qcow2 5" {
		t.Errorf("inner events = %q", got)
	}
}

func TestThinCreateVolumeRefused(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, warmReport())
	inner := &thinInner{}
	m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	_, err := m.CreateVolume("thin-test", "vm1-2.qcow2", 5)
	var ae *app.Error
	if err == nil || !app.AsError(err, &ae) || ae.Kind != app.KindInvalid || !strings.Contains(err.Error(), "add-disk") {
		t.Fatalf("err = %v", err)
	}
	if n := len(h.writes(t)); n != 0 {
		t.Errorf("writes: %q", h.writes(t))
	}
	if p, err := m.CreateVolume("vmtool", "d.qcow2", 5); err != nil || p != "/var/lib/vmtool/images/d.qcow2" {
		t.Errorf("libvirt pool: %q, %v", p, err)
	}
	if got := h.calls(t); len(got) != 1 { // only the refused call's report
		t.Errorf("helper calls = %q", got)
	}
}

// ---- lookups for the server ----

func TestThinVolumeLookup(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_vol"), thinLV(tplLV, 10*gib, tplTagsA),
		thinLV(bldLV, gib, "vmtool_building"), thinLV("foreign", gib, ""), lvRow("thick", "-wi-ao----", gib, "", "")))
	m := newThinFor(t, &thinInner{}, newCache(t, uint64(gib), sha64a))
	for _, tc := range []struct {
		p     string
		kind  string
		found bool
	}{
		{"/dev/vg-test/vm1", vmtool.ThinKindVol, true},
		{"/dev/vg-test/" + tplLV, vmtool.ThinKindTemplate, true},
		{"/dev/vg-test/" + bldLV, vmtool.ThinKindBuilding, true},
		{"/dev/vg-test/foreign", "", false},
		{"/dev/vg-test/thick", "", false},
		{"/dev/vg-test/nope", "", false},
		{"/dev/other/vm1", "", false},
		{"/dev/vg-test/../vm1", "", false},
		{"/dev/mapper/vg--test-vm1", "", false},
		{"/var/lib/vmtool/images/a", "", false},
	} {
		kind, found, err := m.ThinVolume(tc.p)
		if err != nil || found != tc.found || kind != tc.kind {
			t.Errorf("ThinVolume(%q) = %q, %v, %v; want %q, %v", tc.p, kind, found, err, tc.kind, tc.found)
		}
	}
	// a path that is not /dev/<vg>/<lv> does not run the helper
	before := h.count(t, "report")
	m.ThinVolume("/var/lib/vmtool/images/a")
	if h.count(t, "report") != before {
		t.Error("helper ran for a path that is not an LV path")
	}
}

func TestSavedTemplatePath(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV(savedLV, 10*gib, savedTags), thinLV(tplLV, 10*gib, tplTagsA)))
	m := newThinFor(t, &thinInner{}, newCache(t, uint64(gib), sha64a))
	p, err := m.SavedTemplatePath("proj")
	if err != nil || p != "/dev/vg-test/"+savedLV {
		t.Fatalf("SavedTemplatePath(proj) = %q, %v", p, err)
	}
	// an image template whose image left the cache is stale and not used
	if _, err := m.SavedTemplatePath("img1"); !vmtool.IsNotFound(err) {
		t.Errorf("image template answered: %v", err)
	}
	if _, err := m.SavedTemplatePath("nope"); !vmtool.IsNotFound(err) {
		t.Errorf("err = %v, want not found", err)
	}
	// a helper that cannot run is "not found" too, never a different error
	h.write(t, "fail_report", "sudo: a password is required\n")
	if _, err := m.SavedTemplatePath("proj"); !vmtool.IsNotFound(err) {
		t.Errorf("helper failure: %v", err)
	}
}

// ---- start-up ----

func TestCleanBuildingRemovesOnlyBuildingLVs(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_vol"), thinLV(tplLV, 10*gib, tplTagsA),
		thinLV("vmtbld-111111111111", gib, "vmtool_building"), thinLV("vmtbld-222222222222", gib, "vmtool_building,vmtool_template"),
		thinLV("foreign", gib, "")))
	var buf bytes.Buffer
	n := CleanThinBuilding(&vmtool.Thin{Helper: thinHelper}, logger(&buf))
	if n != 2 {
		t.Errorf("removed %d", n)
	}
	eqStrings(t, "calls", h.calls(t), []string{"report", "remove vg-test vmtbld-111111111111", "remove vg-test vmtbld-222222222222"})
	if strings.Count(buf.String(), "\n") != 2 || !strings.Contains(buf.String(), "vmtbld-111111111111") {
		t.Errorf("log = %q", buf.String())
	}

	// a failing helper is one line, never fatal
	h = newHelperStub(t)
	h.write(t, "fail_report", "sudo: a password is required\n")
	buf.Reset()
	if n := CleanThinBuilding(&vmtool.Thin{Helper: thinHelper}, logger(&buf)); n != 0 {
		t.Errorf("removed %d", n)
	}
	if strings.Count(buf.String(), "\n") != 1 {
		t.Errorf("log = %q", buf.String())
	}
	// a failing removal is one line and the next is still tried
	h = newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vmtbld-111111111111", gib, "vmtool_building"), thinLV("vmtbld-222222222222", gib, "vmtool_building")))
	h.write(t, "fail_remove", "vmtool-thin: no\n")
	buf.Reset()
	CleanThinBuilding(&vmtool.Thin{Helper: thinHelper}, logger(&buf))
	if h.count(t, "remove") != 2 || strings.Count(buf.String(), "\n") != 2 {
		t.Errorf("calls %q log %q", h.calls(t), buf.String())
	}
	// nil client: nothing
	if n := CleanThinBuilding(nil, logger(&buf)); n != 0 {
		t.Errorf("nil client removed %d", n)
	}
}

// ---- wrappers keep the optional interfaces ----

// cloneRec is an inner Manager that implements app.TemplateCloner and
// app.ThinHost, recording what the wrappers hand down.
type cloneRec struct {
	app.Manager
	calls []string
}

func (c *cloneRec) CloneImage(base, name, pool string, size uint64) (string, error) {
	c.calls = append(c.calls, fmt.Sprintf("CloneImage %s %s %s %d", base, name, pool, size))
	return "plain", nil
}
func (c *cloneRec) CloneImageFrom(image, base, name, pool string, size uint64) (string, error) {
	c.calls = append(c.calls, fmt.Sprintf("CloneImageFrom %s %s %s %s %d", image, base, name, pool, size))
	return "from", nil
}
func (c *cloneRec) ThinVolume(p string) (string, bool, error) {
	c.calls = append(c.calls, "ThinVolume "+p)
	return vmtool.ThinKindVol, true, nil
}
func (c *cloneRec) SavedTemplatePath(n string) (string, error) {
	c.calls = append(c.calls, "SavedTemplatePath "+n)
	return "/dev/vg-test/" + savedLV, nil
}

func TestWrappersForwardTemplateCloner(t *testing.T) {
	for name, tc := range map[string]struct {
		wrap     func(app.Manager) app.Manager
		pool     string
		wantPool string
	}{
		"hypervisorPools, named":      {func(m app.Manager) app.Manager { return hypervisorPools{m} }, "thin-test", "thin-test"},
		"hypervisorPools, empty":      {func(m app.Manager) app.Manager { return hypervisorPools{m} }, "", vmtool.HypervisorPool},
		"defaultPools, named":         {func(m app.Manager) app.Manager { return withDefaultPools(m, "nvme-thin") }, "thin-test", "thin-test"},
		"defaultPools, empty":         {func(m app.Manager) app.Manager { return withDefaultPools(m, "nvme-thin") }, "", "nvme-thin"},
		"default over hypervisor":     {func(m app.Manager) app.Manager { return withDefaultPools(hypervisorPools{m}, "nvme-thin") }, "", "nvme-thin"},
		"default over hypervisor set": {func(m app.Manager) app.Manager { return withDefaultPools(hypervisorPools{m}, "nvme-thin") }, "x", "x"},
	} {
		inner := &cloneRec{}
		m := tc.wrap(inner)
		tcl, ok := m.(app.TemplateCloner)
		if !ok {
			t.Errorf("%s: lost app.TemplateCloner", name)
			continue
		}
		if p, err := tcl.CloneImageFrom("img1", "/b", "vm1", tc.pool, 7); err != nil || p != "from" {
			t.Errorf("%s: = %q, %v", name, p, err)
		}
		if want := fmt.Sprintf("CloneImageFrom img1 /b vm1 %s 7", tc.wantPool); len(inner.calls) != 1 || inner.calls[0] != want {
			t.Errorf("%s: inner calls %q, want %q", name, inner.calls, want)
		}
		th, ok := m.(app.ThinHost)
		if !ok {
			t.Errorf("%s: lost app.ThinHost", name)
			continue
		}
		if k, found, err := th.ThinVolume("/dev/vg-test/vm1"); err != nil || !found || k != vmtool.ThinKindVol {
			t.Errorf("%s: ThinVolume = %q, %v, %v", name, k, found, err)
		}
		if p, err := th.SavedTemplatePath("proj"); err != nil || p != "/dev/vg-test/"+savedLV {
			t.Errorf("%s: SavedTemplatePath = %q, %v", name, p, err)
		}
	}
}

// A wrapper over a Manager with neither interface falls back: CloneImageFrom
// is CloneImage, ThinVolume finds nothing, SavedTemplatePath is not found.
func TestWrappersFallBackWithoutThin(t *testing.T) {
	for name, wrap := range map[string]func(app.Manager) app.Manager{
		"hypervisorPools": func(m app.Manager) app.Manager { return hypervisorPools{m} },
		"defaultPools":    func(m app.Manager) app.Manager { return withDefaultPools(m, "nvme-thin") },
	} {
		inner := &poolFake{}
		m := wrap(inner)
		tcl, ok := m.(app.TemplateCloner)
		if !ok {
			t.Fatalf("%s: no TemplateCloner", name)
		}
		if _, err := tcl.CloneImageFrom("img1", "/b", "vm1", "p", 1); err != nil {
			t.Fatal(err)
		}
		if len(inner.calls) != 1 || !strings.HasPrefix(inner.calls[0], "CloneImage ") {
			t.Errorf("%s: calls %q", name, inner.calls)
		}
		th := m.(app.ThinHost)
		if _, found, err := th.ThinVolume("/dev/vg-test/vm1"); found || err != nil {
			t.Errorf("%s: ThinVolume = %v, %v", name, found, err)
		}
		if _, err := th.SavedTemplatePath("proj"); !vmtool.IsNotFound(err) {
			t.Errorf("%s: SavedTemplatePath err = %v", name, err)
		}
	}
}

// Through Resolve: a hypervisor with a helper hands a TemplateCloner whose
// default pool is applied, and the thin clone reaches the helper.
func TestResolveKeepsTemplateClonerOverThin(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, warmReport())
	c := newCache(t, uint64(10*gib), sha64a)
	local := func() (app.Manager, func(), error) { return &thinInner{}, func() {}, nil }
	r := NewWith("hv1", config.RoleHypervisor, nil, local).WithThin(&vmtool.Thin{Helper: thinHelper}).
		WithImageManifest(c.manifest).WithDefaultPools("thin-test", nil)
	m, done, err := r.Resolve("hv1")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	tcl, ok := m.(app.TemplateCloner)
	if !ok {
		t.Fatal("the resolved hypervisor Manager lost app.TemplateCloner")
	}
	if p, err := tcl.CloneImageFrom("img1", c.img, "vm1", "", gb20); err != nil || p != "/dev/vg-test/vm1" {
		t.Fatalf("clone = %q, %v", p, err)
	}
	eqStrings(t, "helper argv", h.calls(t), argvWarm20())
	if _, ok := m.(app.ThinHost); !ok {
		t.Error("the resolved hypervisor Manager lost app.ThinHost")
	}
}

// tplStore is an inner Manager that caches images and can ask for a template:
// withDefaultPools wraps it in storeDefaultPools / fetchDefaultPools, which
// must keep app.TemplateFinder visible.
type tplStore struct {
	app.Manager
	asked []string
}

func (f *tplStore) TemplatePath(n string) (string, error) {
	f.asked = append(f.asked, n)
	return "/dev/vg-test/" + savedLV, nil
}
func (f *tplStore) ImageManifest(string) (app.RemoteImage, error) { return app.RemoteImage{}, nil }
func (f *tplStore) UploadImage(context.Context, string, io.Reader, int64, string, func(int64)) error {
	return nil
}

func TestDefaultPoolsKeepTemplateFinder(t *testing.T) {
	plain := withDefaultPools(&poolFake{}, "nvme-thin")
	tf, ok := plain.(app.TemplateFinder)
	if !ok {
		t.Fatal("defaultPools lost app.TemplateFinder")
	}
	if _, err := tf.TemplatePath("proj"); !vmtool.IsNotFound(err) {
		t.Errorf("over a Manager that cannot ask: %v", err)
	}
	inner := &tplStore{}
	m := withDefaultPools(inner, "nvme-thin")
	if _, ok := m.(app.ImageStore); !ok {
		t.Fatal("test fixture: not an ImageStore")
	}
	tf, ok = m.(app.TemplateFinder)
	if !ok {
		t.Fatal("storeDefaultPools lost app.TemplateFinder")
	}
	if p, err := tf.TemplatePath("proj"); err != nil || p != "/dev/vg-test/"+savedLV || len(inner.asked) != 1 {
		t.Errorf("TemplatePath = %q, %v, asked %q", p, err, inner.asked)
	}
}
