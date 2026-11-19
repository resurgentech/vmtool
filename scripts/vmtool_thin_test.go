package scripts_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The helper deploy/vmtool-thin runs as root on a hypervisor. These tests run
// a COPY of it with bash, in which the one PATH line is replaced by a
// directory of stubs (lvs, lvcreate, ... qemu-img, setpriv, chown, all
// recording their argv) and the one pool-directory placeholder by a temp
// directory. The installed script has no environment override at all. The
// stub directory holds symlinks to bash, env, dirname and cat and nothing
// else, so a tool the script was never meant to call is "command not found".
// No real lvm, qemu-img or chown ever runs here.

const (
	thinScript   = "../deploy/vmtool-thin"
	thinPathLine = "PATH=/usr/sbin:/usr/bin:/sbin:/bin"
	thinPoolTok  = "@POOL_DIR@"
	thinVG       = "vg-test"
)

// writers are the stubs that must record nothing when the helper refuses.
var thinWriters = []string{"lvcreate", "lvchange", "lvextend", "lvremove", "lvrename", "qemu-img", "setpriv", "chown"}

// lvs answers: --reportformat from lvs.json; the all-VG discovery listing
// (-o vg_name,...) from lvs.all.out; the per-VG check listing from
// lvs.<VG>.out, and fails like lvm does for an unknown VG.
const thinLvsBody = `case "$*" in
  *--reportformat*) cat "$d/lvs.json" ;;
  *"-o vg_name"*) cat "$d/lvs.all.out" ;;
  *) for last; do :; done
     f="$d/lvs.$last.out"
     [[ -f $f ]] || { echo "Volume group not found" >&2; exit 5; }
     cat "$f" ;;
esac`

// Fixture: the `|`-separated listing lvs --noheadings prints (leading blanks
// as lvm prints them).
const thinListing = `  thin-test|twi-aotz--|64424509440|vmtool_thinpool
  vm1|Vwi-a-tz--|10737418240|vmtool_vol
  vm1-2|Vwi-a-tz--|1073741824|vmtool_vol,vmtool_name=x
  vmtpl-000000000001|Vwi-a-tz--|10737418240|vmtool_template,vmtool_name=img1,vmtool_sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  bld|Vwi---tz--|10737418240|vmtool_building
  foreign|-wi-a-----|1073741824|
  other|Vwi-a-tz--|1073741824|someone_else,vmtool_volume
  fakepool|twi-a-tz--|64424509440|vmtool_vol
`

const thinListingNoPool = `  data|-wi-a-----|1073741824|
  vm1|-wi-a-----|1073741824|vmtool_vol
`

// thinTemp is a fresh directory with a plain name: t.TempDir embeds the test
// name, which may hold a $ that the script copy's rewritten lines would expand.
func thinTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "thin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type thinEnv struct {
	t      *testing.T
	stub   string
	images string
	script string
}

func newThinEnv(t *testing.T) *thinEnv { return newThinEnvRender(t, thinPoolDirOK) }

// poolDirOK in newThinEnvRender means: render the temp image dir.
const thinPoolDirOK = "\x00images"

// newThinEnvRender writes the script copy. pool is what replaces the
// placeholder (thinPoolDirOK: the temp image dir; "keep": left as it is).
func newThinEnvRender(t *testing.T, pool string) *thinEnv {
	t.Helper()
	src, err := os.ReadFile(thinScript)
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if n := strings.Count(s, thinPathLine); n != 1 {
		t.Fatalf("script has %d occurrences of the PATH line, want exactly 1", n)
	}
	if n := strings.Count(s, thinPoolTok); n != 1 {
		t.Fatalf("script has %d occurrences of the pool placeholder, want exactly 1", n)
	}
	stub := thinTemp(t)
	addStub(t, stub, "lvs", thinLvsBody)
	for _, w := range thinWriters {
		body := "true"
		if w == "setpriv" {
			// Run what follows the "--": the stub qemu-img then records too.
			body = `while [[ $# -gt 0 && $1 != -- ]]; do shift; done; shift; exec "$@"`
		}
		addStub(t, stub, w, body)
	}
	for _, tool := range []string{"bash", "env", "dirname", "cat"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(stub, tool)); err != nil {
			t.Fatal(err)
		}
	}
	images := thinTemp(t)
	s = strings.Replace(s, thinPathLine, "PATH="+stub, 1)
	switch pool {
	case thinPoolDirOK:
		s = strings.Replace(s, thinPoolTok, images, 1)
	case "keep":
	default:
		s = strings.Replace(s, thinPoolTok, pool, 1)
	}
	script := filepath.Join(thinTemp(t), "vmtool-thin")
	if err := os.WriteFile(script, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &thinEnv{t: t, stub: stub, images: images, script: script}
	e.fixture(thinVG, thinListing)
	e.fixture("vg-nopool", thinListingNoPool)
	e.write("lvs.all.out", "  "+thinVG+"|twi-aotz--|vmtool_thinpool\n  "+thinVG+"|Vwi-a-tz--|vmtool_vol\n  vg-nopool|-wi-a-----|\n")
	e.write("lvs.json", `{"report":[{"lv":[{"lv_name":"vm1"}]}]}`+"\n")
	return e
}

func (e *thinEnv) write(name, body string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.stub, name), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *thinEnv) fixture(vg, listing string) { e.write("lvs."+vg+".out", listing) }

// img makes a regular file in the image directory and returns its path.
func (e *thinEnv) img(name string) string {
	e.t.Helper()
	p := filepath.Join(e.images, name)
	if err := os.WriteFile(p, []byte("PLACEHOLDER-not-an-image"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *thinEnv) run(args ...string) result {
	e.t.Helper()
	cmd := exec.Command("bash", append([]string{e.script}, args...)...)
	cmd.Dir = thinTemp(e.t)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + cmd.Dir}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return result{so.String(), se.String(), code}
}

func (e *thinEnv) rec(name string) string {
	return readOr(e.t, filepath.Join(e.stub, name+".argv"))
}

func (e *thinEnv) noWrites() {
	e.t.Helper()
	for _, w := range thinWriters {
		if got := e.rec(w); got != "" {
			e.t.Errorf("%s ran: %q", w, got)
		}
	}
}

// refuses: non-zero exit, exactly one stderr line, nothing written.
func (e *thinEnv) refuses(args ...string) {
	e.t.Helper()
	r := e.run(args...)
	if r.code == 0 {
		e.t.Errorf("exit 0, want a refusal: %q", r.stdout)
	}
	line := strings.TrimSuffix(r.stderr, "\n")
	if line == "" || strings.Contains(line, "\n") || !strings.HasPrefix(line, "vmtool-thin: ") {
		e.t.Errorf("want one stderr line prefixed %q, got %q", "vmtool-thin: ", r.stderr)
	}
	if r.stdout != "" {
		e.t.Errorf("stdout on a refusal: %q", r.stdout)
	}
	e.noWrites()
}

// ok: exit 0 and no output.
func (e *thinEnv) ok(args ...string) {
	e.t.Helper()
	r := e.run(args...)
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		e.t.Fatalf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

func (e *thinEnv) want(name, argv string) {
	e.t.Helper()
	if got := e.rec(name); got != argv {
		e.t.Errorf("%s argv:\n got %q\nwant %q", name, got, argv)
	}
}

// ---------------------------------------------------------------- happy paths

func TestThinCreate(t *testing.T) {
	e := newThinEnv(t)
	e.ok("create", thinVG, "thin-test", "vm9", "10737418240", "vmtool_vol", "vmtool_name=img1")
	e.want("lvcreate", "-V 10737418240b --thin -n vm9 --addtag vmtool_vol --addtag vmtool_name=img1 vg-test/thin-test\n")
	e.want("lvchange", "")
}

func TestThinSnapshot(t *testing.T) {
	e := newThinEnv(t)
	e.ok("snapshot", thinVG, "vmtpl-000000000001", "vm9", "vmtool_vol")
	e.want("lvcreate", "-s -kn -n vm9 --addtag vmtool_vol vg-test/vmtpl-000000000001\n")
	e.want("lvchange", "-ay vg-test/vm9\n")
}

func TestThinExtend(t *testing.T) {
	e := newThinEnv(t)
	e.ok("extend", thinVG, "vm1", "21474836480")
	e.want("lvextend", "-L 21474836480b vg-test/vm1\n")
}

// BYTES equal to the current size succeeds and does nothing.
func TestThinExtendSameSizeIsANoop(t *testing.T) {
	e := newThinEnv(t)
	e.ok("extend", thinVG, "vm1", "10737418240")
	e.noWrites()
}

func TestThinRemove(t *testing.T) {
	e := newThinEnv(t)
	e.ok("remove", thinVG, "vm1")
	e.want("lvremove", "-y vg-test/vm1\n")
	for _, n := range []string{"vmtpl-000000000001", "bld"} {
		e.ok("remove", thinVG, n)
	}
}

func TestThinRename(t *testing.T) {
	e := newThinEnv(t)
	e.ok("rename", thinVG, "bld", "vmtpl-000000000009")
	e.want("lvrename", "vg-test bld vmtpl-000000000009\n")
}

func TestThinTag(t *testing.T) {
	e := newThinEnv(t)
	e.ok("tag", thinVG, "bld", "+vmtool_template", "-vmtool_building", "+vmtool_name=img1")
	e.want("lvchange", "--addtag vmtool_template --deltag vmtool_building --addtag vmtool_name=img1 vg-test/bld\n")
}

// import: the device node goes to vmtool, qemu-img runs as vmtool through
// setpriv (never as root), the node goes back.
func TestThinImport(t *testing.T) {
	e := newThinEnv(t)
	src := e.img("img1")
	e.ok("import", thinVG, "bld", src)
	e.want("chown", "vmtool:vmtool /dev/vg-test/bld\nroot:disk /dev/vg-test/bld\n")
	e.want("setpriv", "--reuid=vmtool --regid=vmtool --init-groups -- qemu-img convert -f qcow2 -n -O raw -t none -W "+src+" /dev/vg-test/bld\n")
	e.want("qemu-img", "convert -f qcow2 -n -O raw -t none -W "+src+" /dev/vg-test/bld\n")
}

func TestThinExport(t *testing.T) {
	e := newThinEnv(t)
	dest := filepath.Join(e.images, ".img1.part")
	e.ok("export", thinVG, "vmtpl-000000000001", dest)
	e.want("chown", "vmtool:vmtool /dev/vg-test/vmtpl-000000000001\nroot:disk /dev/vg-test/vmtpl-000000000001\n")
	e.want("setpriv", "--reuid=vmtool --regid=vmtool --init-groups -- qemu-img convert -f raw -O qcow2 /dev/vg-test/vmtpl-000000000001 "+dest+"\n")
	e.want("qemu-img", "convert -f raw -O qcow2 /dev/vg-test/vmtpl-000000000001 "+dest+"\n")
}

// A failing qemu-img still gives the device node back, and the helper fails.
func TestThinImportRestoresOwnerOnFailure(t *testing.T) {
	e := newThinEnv(t)
	addStub(t, e.stub, "qemu-img", "exit 3")
	r := e.run("import", thinVG, "bld", e.img("img1"))
	if r.code == 0 {
		t.Fatal("exit 0 although qemu-img failed")
	}
	e.want("chown", "vmtool:vmtool /dev/vg-test/bld\nroot:disk /dev/vg-test/bld\n")
}

// report: lvs's JSON straight through, over the VGs holding a pool only.
func TestThinReport(t *testing.T) {
	e := newThinEnv(t)
	r := e.run("report")
	if r.code != 0 || r.stderr != "" || r.stdout != `{"report":[{"lv":[{"lv_name":"vm1"}]}]}`+"\n" {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	argv := e.rec("lvs")
	want := "--reportformat json --units b --nosuffix -o lv_name,vg_name,lv_attr,lv_size,pool_lv,origin,data_percent,metadata_percent,lv_tags,lv_time vg-test\n"
	if !strings.HasSuffix(argv, want) || strings.Contains(argv, "vg-nopool") {
		t.Errorf("lvs argv %q", argv)
	}
	e.noWrites()
}

// No VG holds a pool: an empty but valid lvs-shaped report, no lvs report call.
func TestThinReportNoPool(t *testing.T) {
	e := newThinEnv(t)
	e.write("lvs.all.out", "  vg-nopool|-wi-a-----|\n")
	r := e.run("report")
	if r.code != 0 || r.stderr != "" || strings.TrimSpace(r.stdout) != `{"report": [{"lv": []}]}` {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	if strings.Contains(e.rec("lvs"), "--reportformat") {
		t.Error("lvs report ran with no VG")
	}
}

// ------------------------------------------------------------------ refusals

func TestThinRefusesBadNames(t *testing.T) {
	long := "a" + strings.Repeat("b", 100) // 101 characters
	names := []string{"", "-x", ".x", "_x", "a b", "a/b", "a;b", "a$b", "a\nb", "a\n", long, "a`id`", "a=b", "..", "é"}
	for _, n := range names {
		t.Run("name "+strings.ReplaceAll(n, "\n", `\n`), func(t *testing.T) {
			e := newThinEnv(t)
			e.refuses("create", thinVG, "thin-test", n, "1024", "vmtool_vol")
		})
	}
	for _, n := range []string{"-vg", "vg/x", "vg x", "", "a\n"} {
		t.Run("vg "+strings.ReplaceAll(n, "\n", `\n`), func(t *testing.T) {
			e := newThinEnv(t)
			e.refuses("create", n, "thin-test", "vm9", "1024", "vmtool_vol")
			e.refuses("remove", n, "vm1")
		})
	}
	// Every verb validates its names, new and old.
	e := newThinEnv(t)
	e.refuses("snapshot", thinVG, "vm1", "a b", "vmtool_vol")
	e.refuses("rename", thinVG, "vm1", "a b")
	e.refuses("rename", thinVG, "a b", "vm9")
	e.refuses("remove", thinVG, "a b")
	e.refuses("extend", thinVG, "a b", "1024")
	e.refuses("tag", thinVG, "a b", "+vmtool_x")
}

func TestThinRefusesReservedNames(t *testing.T) {
	for _, s := range []string{"snapshot", "pvmove", "_cdata", "_cmeta", "_corig", "_mlog", "_mimage", "_pmspare", "_rimage", "_rmeta", "_tdata", "_tmeta", "_vorigin", "_vdata"} {
		t.Run(s, func(t *testing.T) {
			e := newThinEnv(t)
			for _, n := range []string{s, "a" + s, s + "z", "vm" + s + "9"} {
				e.refuses("create", thinVG, "thin-test", n, "1024", "vmtool_vol")
				e.refuses("snapshot", thinVG, "vm1", n, "vmtool_vol")
				e.refuses("rename", thinVG, "vm1", n)
			}
		})
	}
}

func TestThinRefusesVGWithoutPool(t *testing.T) {
	e := newThinEnv(t)
	e.refuses("create", "vg-nopool", "thin-test", "vm9", "1024", "vmtool_vol")
	// vm1 there carries vmtool_vol, but the VG holds no pool: still nothing.
	e.refuses("remove", "vg-nopool", "vm1")
	e.refuses("extend", "vg-nopool", "vm1", "2048")
	e.refuses("rename", "vg-nopool", "vm1", "vm2")
	e.refuses("tag", "vg-nopool", "vm1", "+vmtool_x")
	e.refuses("snapshot", "vg-nopool", "vm1", "vm2", "vmtool_vol")
	e.refuses("import", "vg-nopool", "vm1", e.img("img1"))
	e.refuses("export", "vg-nopool", "vm1", filepath.Join(e.images, ".x.part"))
	// A VG lvs does not know.
	e.refuses("remove", "vg-unknown", "vm1")
}

// A pool tag on a thick LV, or a thin pool without the tag, is not "the pool".
func TestThinPoolNeedsTagAndAttr(t *testing.T) {
	e := newThinEnv(t)
	e.fixture("vg-odd", "  tp|twi-a-tz--|1073741824|\n  fake|-wi-a-----|1073741824|vmtool_thinpool\n  vm1|Vwi-a-tz--|1024|vmtool_vol\n")
	e.refuses("remove", "vg-odd", "vm1")
	e.refuses("create", "vg-odd", "tp", "vm9", "1024", "vmtool_vol")
	e.refuses("create", "vg-odd", "fake", "vm9", "1024", "vmtool_vol")
}

func TestThinRefusesPoolThatIsNotThePool(t *testing.T) {
	for _, p := range []string{"vm1", "foreign", "nope", "fakepool", "THIN-TEST", "thin-test2"} {
		t.Run(p, func(t *testing.T) {
			e := newThinEnv(t)
			e.refuses("create", thinVG, p, "vm9", "1024", "vmtool_vol")
		})
	}
}

func TestThinRefusesUntaggedTarget(t *testing.T) {
	// foreign: no tags. other: a tag that merely contains vmtool_vol. nope:
	// not there. thin-test: the pool itself.
	for _, n := range []string{"foreign", "other", "nope", "thin-test"} {
		t.Run(n, func(t *testing.T) {
			e := newThinEnv(t)
			src := e.img("img1")
			dest := filepath.Join(e.images, ".o.part")
			e.refuses("snapshot", thinVG, n, "vm9", "vmtool_vol")
			e.refuses("extend", thinVG, n, "999999999999")
			e.refuses("remove", thinVG, n)
			e.refuses("rename", thinVG, n, "vm9")
			e.refuses("tag", thinVG, n, "+vmtool_x")
			e.refuses("import", thinVG, n, src)
			e.refuses("export", thinVG, n, dest)
		})
	}
}

// A tagged thin pool (tag set by hand) is a pool all the same: nothing acts on it.
func TestThinRefusesPoolAsTarget(t *testing.T) {
	e := newThinEnv(t)
	dest := filepath.Join(e.images, ".o.part")
	for _, n := range []string{"fakepool", "thin-test"} {
		e.refuses("remove", thinVG, n)
		e.refuses("extend", thinVG, n, "999999999999")
		e.refuses("rename", thinVG, n, "vm9")
		e.refuses("tag", thinVG, n, "-vmtool_vol")
		e.refuses("snapshot", thinVG, n, "vm9", "vmtool_vol")
		e.refuses("import", thinVG, n, e.img("img1"))
		e.refuses("export", thinVG, n, dest)
	}
}

func TestThinSnapshotSourceMustBeThinVolume(t *testing.T) {
	e := newThinEnv(t)
	e.fixture("vg-odd", "  tp|twi-aotz--|1073741824|vmtool_thinpool\n  thick|-wi-a-----|1073741824|vmtool_vol\n")
	e.refuses("snapshot", "vg-odd", "thick", "vm9", "vmtool_vol")
}

// FOREMAN: a bad VG name is refused before lvs is asked anything, even when
// lvs would answer for it.
func TestThinBadVGNameNeverReachesLvs(t *testing.T) {
	for _, vg := range []string{"-vg-test", "_vg-test", ".vg-test", "vg:test"} {
		t.Run(vg, func(t *testing.T) {
			e := newThinEnv(t)
			e.fixture(vg, thinListing)
			e.refuses("remove", vg, "vm1")
			e.refuses("create", vg, "thin-test", "vm9", "1024", "vmtool_vol")
			if got := e.rec("lvs"); got != "" {
				t.Errorf("lvs ran for VG %q: %q", vg, got)
			}
		})
	}
}

// FOREMAN: a source outside the image directory is refused as such (the
// flat-name check would also refuse it; this pins the first line of defence).
func TestThinSrcOutsideNamesTheReason(t *testing.T) {
	e := newThinEnv(t)
	outside := filepath.Join(thinTemp(t), "img1")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.run("import", thinVG, "vm1", outside)
	if r.code == 0 || !strings.Contains(r.stderr, "source is not in the image directory") {
		t.Errorf("exit %d, stderr %q", r.code, r.stderr)
	}
	e.noWrites()
}

// FOREMAN: every target verb acts only on a thin volume; a thick LV someone
// tagged vmtool_vol is still not vmtool's.
func TestThinTargetMustBeThinVolume(t *testing.T) {
	e := newThinEnv(t)
	e.fixture("vg-odd", "  tp|twi-aotz--|1073741824|vmtool_thinpool\n  thick|-wi-a-----|1073741824|vmtool_vol\n")
	e.refuses("remove", "vg-odd", "thick")
	e.refuses("extend", "vg-odd", "thick", "2147483648")
	e.refuses("rename", "vg-odd", "thick", "vm9")
	e.refuses("tag", "vg-odd", "thick", "+vmtool_template")
	e.refuses("import", "vg-odd", "thick", e.img("img1"))
	e.refuses("export", "vg-odd", "thick", filepath.Join(e.images, ".o.part"))
}

func TestThinRefusesBadTags(t *testing.T) {
	bad := []string{"foo", "vmtool_", "vmtool_X", "vmtool_a b", "vmtool_a=", "vmtool_a=b;c", "vmtool_a=b c", "vmtool_a$b", "vmtool_a\nb", "-vmtool_vol", "+vmtool_vol", "vmtool_a==b", "Vmtool_a", "vmtool-a"}
	for _, tg := range bad {
		t.Run(strings.ReplaceAll(tg, "\n", `\n`), func(t *testing.T) {
			e := newThinEnv(t)
			e.refuses("create", thinVG, "thin-test", "vm9", "1024", "vmtool_vol", tg)
			e.refuses("snapshot", thinVG, "vm1", "vm9", "vmtool_vol", tg)
		})
	}
	for _, tg := range []string{"vmtool_a", "foo", "+foo", "-foo", "+vmtool_", "+", "-", "", "+vmtool_a=", "+vmtool_a b", "*vmtool_a"} {
		t.Run("tag "+tg, func(t *testing.T) {
			e := newThinEnv(t)
			e.refuses("tag", thinVG, "vm1", tg)
		})
	}
}

func TestThinRefusesPoolTag(t *testing.T) {
	e := newThinEnv(t)
	for _, tg := range []string{"vmtool_thinpool", "vmtool_thinpool=x"} {
		e.refuses("create", thinVG, "thin-test", "vm9", "1024", "vmtool_vol", tg)
		e.refuses("snapshot", thinVG, "vm1", "vm9", "vmtool_vol", tg)
		e.refuses("tag", thinVG, "vm1", "+"+tg)
		e.refuses("tag", thinVG, "vm1", "-"+tg)
	}
}

func TestThinRefusesNoKindTag(t *testing.T) {
	e := newThinEnv(t)
	e.refuses("create", thinVG, "thin-test", "vm9", "1024", "vmtool_name=x")
	e.refuses("create", thinVG, "thin-test", "vm9", "1024", "vmtool_volume")
	e.refuses("create", thinVG, "thin-test", "vm9", "1024")
	e.refuses("snapshot", thinVG, "vm1", "vm9", "vmtool_name=x")
	e.refuses("snapshot", thinVG, "vm1", "vm9")
	e.refuses("tag", thinVG, "vm1")
}

func TestThinRefusesBadBytes(t *testing.T) {
	for _, b := range []string{"abc", "0", "-5", "1.5", "", "1e3", "0100", "+5", "10 ", "99999999999999999999", "1\n"} {
		t.Run(strings.ReplaceAll(b, "\n", `\n`), func(t *testing.T) {
			e := newThinEnv(t)
			e.refuses("create", thinVG, "thin-test", "vm9", b, "vmtool_vol")
			e.refuses("extend", thinVG, "vm1", b)
		})
	}
}

func TestThinRefusesShrink(t *testing.T) {
	e := newThinEnv(t)
	e.refuses("extend", thinVG, "vm1", "10737418239")
	e.refuses("extend", thinVG, "vm1", "1")
}

func TestThinRefusesExistingNames(t *testing.T) {
	e := newThinEnv(t)
	e.refuses("create", thinVG, "thin-test", "vm1", "1024", "vmtool_vol")
	e.refuses("create", thinVG, "thin-test", "foreign", "1024", "vmtool_vol")
	e.refuses("create", thinVG, "thin-test", "thin-test", "1024", "vmtool_vol")
	e.refuses("snapshot", thinVG, "vm1", "vm1-2", "vmtool_vol")
	e.refuses("snapshot", thinVG, "vm1", "foreign", "vmtool_vol")
	e.refuses("rename", thinVG, "vm1", "vm1-2")
	e.refuses("rename", thinVG, "vm1", "foreign")
	e.refuses("rename", thinVG, "vm1", "thin-test")
}

func TestThinRefusesBadSrc(t *testing.T) {
	e := newThinEnv(t)
	good := e.img("img1")
	outside := filepath.Join(t.TempDir(), "img1")
	os.WriteFile(outside, []byte("x"), 0o644)
	os.Mkdir(filepath.Join(e.images, "sub"), 0o755)
	os.WriteFile(filepath.Join(e.images, "sub", "img1"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(e.images, "adir"), 0o755)
	os.Symlink(outside, filepath.Join(e.images, "link"))
	os.Symlink(good, filepath.Join(e.images, "link2"))
	os.Symlink(filepath.Join(e.images, "missing"), filepath.Join(e.images, "dangling"))
	cases := map[string]string{
		"relative":           "img1",
		"relative dot":       "./img1",
		"outside":            outside,
		"dotdot":             e.images + "/../" + filepath.Base(e.images) + "/img1",
		"dotdot file":        e.images + "/..",
		"subdirectory":       e.images + "/sub/img1",
		"symlink out":        e.images + "/link",
		"symlink in":         e.images + "/link2",
		"dangling":           e.images + "/dangling",
		"directory":          e.images + "/adir",
		"missing":            e.images + "/missing",
		"the dir itself":     e.images,
		"the dir slash":      e.images + "/",
		"double slash":       e.images + "//img1",
		"prefix only":        e.images + "x/img1",
		"leading dot":        e.images + "/.img1",
		"empty":              "",
		"device":             "/dev/null",
		"fifo-ish name":      e.images + "/img1\n",
		"dir then dotdot":    e.images + "/sub/../img1",
		"trailing slash img": e.images + "/img1/",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			e.refuses("import", thinVG, "bld", src)
		})
	}
}

func TestThinRefusesBadDest(t *testing.T) {
	e := newThinEnv(t)
	outside := filepath.Join(t.TempDir(), ".o.part")
	os.Mkdir(filepath.Join(e.images, "sub"), 0o755)
	e.img(".exists.part")
	os.Symlink(filepath.Join(e.images, "nowhere"), filepath.Join(e.images, ".dangling.part"))
	os.Symlink(e.images, filepath.Join(e.images, ".link.part"))
	cases := map[string]string{
		"not .part":    e.images + "/out.qcow2",
		"bare name":    e.images + "/img1",
		"only .part":   e.images + "/.part",
		"exists":       e.images + "/.exists.part",
		"dangling":     e.images + "/.dangling.part",
		"symlink":      e.images + "/.link.part",
		"relative":     ".o.part",
		"outside":      outside,
		"subdirectory": e.images + "/sub/.o.part",
		"dotdot":       e.images + "/../.o.part",
		"dotdot dir":   e.images + "/sub/../.o.part",
		"empty":        "",
		"the dir":      e.images,
		"newline":      e.images + "/.o.part\n",
		"space":        e.images + "/.o p.part",
		"double dot":   e.images + "/..part",
	}
	for name, dest := range cases {
		t.Run(name, func(t *testing.T) {
			e.refuses("export", thinVG, "vm1", dest)
		})
	}
}

func TestThinRefusesWrongArgCounts(t *testing.T) {
	e := newThinEnv(t)
	img := e.img("img1")
	dest := filepath.Join(e.images, ".o.part")
	cases := [][]string{
		{"report", "x"},
		{"create", thinVG, "thin-test", "vm9", "1024"},
		{"create", thinVG, "thin-test", "vm9"},
		{"create"},
		{"snapshot", thinVG, "vm1", "vm9"},
		{"snapshot", thinVG, "vm1"},
		{"extend", thinVG, "vm1"},
		{"extend", thinVG, "vm1", "20000000000", "x"},
		{"remove", thinVG},
		{"remove", thinVG, "vm1", "x"},
		{"remove"},
		{"rename", thinVG, "vm1"},
		{"rename", thinVG, "vm1", "vm9", "x"},
		{"tag", thinVG, "vm1"},
		{"tag", thinVG},
		{"import", thinVG, "bld"},
		{"import", thinVG, "bld", img, "x"},
		{"export", thinVG, "vm1"},
		{"export", thinVG, "vm1", dest, "x"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c, " "), func(t *testing.T) { e.refuses(c...) })
	}
}

func TestThinRefusesUnknownVerb(t *testing.T) {
	e := newThinEnv(t)
	for _, v := range []string{"", "lvcreate", "REPORT", "--help", "-h", "destroy", "report;id", "pool-remove", "wipe"} {
		t.Run("verb "+v, func(t *testing.T) {
			e.refuses(v, thinVG, "vm1")
		})
	}
	e.refuses()
}

// The installed copy has the directory rendered in; a copy that was not
// rendered, or rendered to something that is not an absolute plain path,
// does nothing, not even report.
func TestThinRefusesUnrenderedPoolDir(t *testing.T) {
	for name, pool := range map[string]string{
		"unrendered": "keep", "relative": "rel/dir", "empty": "", "dotdot": "/var/../etc",
		"at sign": "/var/@x", "space": "/var/a b", "root": "/",
	} {
		t.Run(name, func(t *testing.T) {
			e := newThinEnvRender(t, pool)
			e.refuses("report")
			e.refuses("remove", thinVG, "vm1")
			if got := e.rec("lvs"); got != "" {
				t.Errorf("lvs ran: %q", got)
			}
		})
	}
}

// -------------------------------------------------------------- script text

func TestThinScriptText(t *testing.T) {
	b, err := os.ReadFile(thinScript)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "#!/bin/bash\n") {
		t.Error("first line is not #!/bin/bash")
	}
	if n := strings.Count(s, thinPathLine); n != 1 {
		t.Errorf("PATH line occurs %d times, want 1", n)
	}
	if n := len(regexp.MustCompile(`(?m)^\s*(export\s+)?PATH=`).FindAllString(s, -1)); n != 1 {
		t.Errorf("%d lines assign PATH, want 1", n)
	}
	if n := strings.Count(s, thinPoolTok); n != 1 {
		t.Errorf("pool placeholder occurs %d times, want 1", n)
	}
	if !strings.Contains(s, "set -euo pipefail") {
		t.Error("no set -euo pipefail")
	}
	if !strings.Contains(s, "umask 022") || !strings.Contains(s, "LC_ALL=C") {
		t.Error("umask 022 / LC_ALL=C missing")
	}
	if regexp.MustCompile(`\beval\b`).MatchString(s) {
		t.Error("eval in the script")
	}
	var setpriv, qemu int
	for i, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if strings.Contains(l, "lvremove") && (strings.Contains(l, "--force") || strings.Contains(l, " -f ") || strings.Contains(l, " -ff")) {
			t.Errorf("line %d forces lvremove: %s", i+1, l)
		}
		if strings.Contains(l, "--force") {
			t.Errorf("line %d: --force: %s", i+1, l)
		}
		if strings.Contains(l, "qemu-img") {
			qemu++
			if !strings.Contains(l, "setpriv") || !strings.Contains(l, "--reuid=vmtool") {
				t.Errorf("line %d runs qemu-img not behind setpriv --reuid=vmtool: %s", i+1, l)
			}
		}
		if strings.Contains(l, "setpriv") {
			setpriv++
		}
		// No root chown of a file path: only the device node.
		if regexp.MustCompile(`\bchown\b`).MatchString(l) && !strings.Contains(l, "/dev/") {
			t.Errorf("line %d: chown of something that is not a device node: %s", i+1, l)
		}
	}
	if qemu != 2 || setpriv != 2 {
		t.Errorf("qemu-img on %d lines, setpriv on %d; want 2 and 2 (import, export)", qemu, setpriv)
	}
}
