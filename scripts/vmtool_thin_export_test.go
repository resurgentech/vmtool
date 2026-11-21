package scripts_test

import (
	"strings"
	"testing"
)

// Tests of the thick export volumes: the export and send verbs of
// deploy/vmtool-thin, and remove and tag on a vmtool_export volume. Same
// stubs as vmtool_thin_test.go; nothing real runs.

const (
	thinTplDev  = "/dev/vg-test/vmtpl-000000000001"
	thinExpDev  = "/dev/vg-test/vmexp-000000000002"
	thinExpNew  = "vmexp-000000000002"
	thinSetpriv = "--reuid=vmtool --regid=vmtool --init-groups -- "
	thinSum     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var thinExportArgs = []string{"export", thinVG, "vmtpl-000000000001", thinExpNew,
	"vmtool_export", "vmtool_name=img1", "vmtool_from=vmtpl-000000000001"}

const thinBothBack = "vmtool:vmtool " + thinTplDev + "\nvmtool:vmtool " + thinExpDev + "\nroot:disk " + thinTplDev + "\nroot:disk " + thinExpDev + "\n"

func TestThinExport(t *testing.T) {
	e := newThinEnv(t)
	r := e.run(thinExportArgs...)
	if r.code != 0 || r.stderr != "" {
		t.Fatalf("exit %d, stderr %q", r.code, r.stderr)
	}
	// Only qemu-img's progress on stdout.
	if r.stdout != "    (1.00/100%)\r" {
		t.Errorf("stdout %q", r.stdout)
	}
	// 10737418241 rounded up to a multiple of the 4194304-byte extent.
	e.want("lvcreate", "-L 10741612544b -n "+thinExpNew+" --addtag vmtool_export --addtag vmtool_name=img1 --addtag vmtool_from=vmtpl-000000000001 vg-test\n")
	e.want("lvchange", "--addtag vmtool_bytes=1310720 vg-test/"+thinExpNew+"\n")
	e.want("lvremove", "")
	e.want("chown", thinBothBack)
	e.want("setpriv",
		thinSetpriv+"qemu-img measure -f raw -O qcow2 --output=json "+thinTplDev+"\n"+
			thinSetpriv+"qemu-img convert -p -f raw -O qcow2 -t none -W -m 16 "+thinTplDev+" "+thinExpDev+"\n"+
			thinSetpriv+"qemu-img check --output=json "+thinExpDev+"\n")
}

// No room: the numbers on one stderr line, no lvcreate, no remove, the
// template's node given back.
func TestThinExportNoRoom(t *testing.T) {
	e := newThinEnv(t)
	// 15 GB free less 10741612544 needed leaves 4258387456, under the reserve
	// of 6442450944 (a tenth of the 64424509440-byte pool).
	e.write("vgs.out", "  15000000000|4194304\n")
	r := e.run(thinExportArgs...)
	if r.code == 0 {
		t.Fatal("exit 0 with no room")
	}
	want := "vmtool-thin: no room: need=10741612544 free=15000000000 reserve=6442450944\n"
	if r.stderr != want || r.stdout != "" {
		t.Errorf("stderr %q stdout %q, want stderr %q", r.stderr, r.stdout, want)
	}
	e.want("lvcreate", "")
	e.want("lvremove", "")
	e.want("lvchange", "")
	e.want("chown", "vmtool:vmtool "+thinTplDev+"\nroot:disk "+thinTplDev+"\n")
}

func TestThinExportRoomExactlyAtReserve(t *testing.T) {
	e := newThinEnv(t)
	e.write("vgs.out", "  17184063488|4194304\n") // 10741612544 + 6442450944
	if r := e.run(thinExportArgs...); r.code != 0 {
		t.Fatalf("exit %d: %q", r.code, r.stderr)
	}
	e.write("vgs.out", "  17184063487|4194304\n")
	if r := e.run(thinExportArgs...); r.code == 0 {
		t.Fatal("one byte short was accepted")
	}
}

// Any failure after lvcreate removes the new volume and only it; both device
// nodes go back.
func TestThinExportFailureAfterCreateRemovesNewVolume(t *testing.T) {
	for name, stub := range map[string]struct{ tool, body string }{
		"convert":      {"qemu-img", `[[ $1 == convert ]] && exit 3; ` + thinQemuBody},
		"check":        {"qemu-img", `[[ $1 == check ]] && exit 3; ` + thinQemuBody},
		"check output": {"qemu-img", `[[ $1 == check ]] && { echo '{}'; exit 0; }; ` + thinQemuBody},
		"tag":          {"lvchange", `exit 4`},
	} {
		t.Run(name, func(t *testing.T) {
			e := newThinEnv(t)
			addStub(t, e.stub, stub.tool, stub.body)
			if r := e.run(thinExportArgs...); r.code == 0 {
				t.Fatal("exit 0 although a step failed")
			}
			e.want("lvremove", "-y vg-test/"+thinExpNew+"\n")
			e.want("chown", thinBothBack)
		})
	}
}

// Failures before lvcreate remove nothing (never the template) and give the
// template's node back.
func TestThinExportFailureBeforeCreate(t *testing.T) {
	for name, stub := range map[string]struct{ tool, body string }{
		"measure":     {"qemu-img", `exit 3`},
		"bad measure": {"qemu-img", `echo '{"required": 1}'`},
		"vgs":         {"vgs", `exit 3`},
		"lvcreate":    {"lvcreate", `exit 5`},
	} {
		t.Run(name, func(t *testing.T) {
			e := newThinEnv(t)
			addStub(t, e.stub, stub.tool, stub.body)
			if r := e.run(thinExportArgs...); r.code == 0 {
				t.Fatal("exit 0 although a step failed")
			}
			e.want("lvremove", "")
			e.want("lvchange", "")
			if got := e.rec("chown"); got != "vmtool:vmtool "+thinTplDev+"\nroot:disk "+thinTplDev+"\n" {
				t.Errorf("chown %q", got)
			}
		})
	}
}

func TestThinExportRefusals(t *testing.T) {
	const tpl = "vmtpl-000000000001"
	cases := map[string][]string{
		"template is a volume":  {"export", thinVG, "vm1", thinExpNew, "vmtool_export"},
		"template is building":  {"export", thinVG, "bld", thinExpNew, "vmtool_export"},
		"template is absent":    {"export", thinVG, "nope", thinExpNew, "vmtool_export"},
		"template is thick":     {"export", thinVG, "vmexp-000000000001", thinExpNew, "vmtool_export"},
		"template is the pool":  {"export", thinVG, "thin-test", thinExpNew, "vmtool_export"},
		"name exists":           {"export", thinVG, tpl, "vmexp-000000000001", "vmtool_export"},
		"name not vmexp":        {"export", thinVG, tpl, "vm9", "vmtool_export"},
		"name upper hex":        {"export", thinVG, tpl, "vmexp-00000000000A", "vmtool_export"},
		"name short":            {"export", thinVG, tpl, "vmexp-00000000002", "vmtool_export"},
		"name long":             {"export", thinVG, tpl, "vmexp-0000000000022", "vmtool_export"},
		"name is the template":  {"export", thinVG, tpl, tpl, "vmtool_export"},
		"no export tag":         {"export", thinVG, tpl, thinExpNew, "vmtool_name=img1"},
		"no tags":               {"export", thinVG, tpl, thinExpNew},
		"bad tag":               {"export", thinVG, tpl, thinExpNew, "vmtool_export", "x y"},
		"foreign tag":           {"export", thinVG, tpl, thinExpNew, "vmtool_export", "other"},
		"pool tag":              {"export", thinVG, tpl, thinExpNew, "vmtool_export", "vmtool_thinpool"},
		"vol tag":               {"export", thinVG, tpl, thinExpNew, "vmtool_export", "vmtool_vol"},
		"template tag":          {"export", thinVG, tpl, thinExpNew, "vmtool_export", "vmtool_template"},
		"building tag":          {"export", thinVG, tpl, thinExpNew, "vmtool_export", "vmtool_building"},
		"bytes tag from caller": {"export", thinVG, tpl, thinExpNew, "vmtool_export", "vmtool_bytes=5"},
		"vg without pool":       {"export", "vg-nopool", "vm1", thinExpNew, "vmtool_export"},
		"old file form":         {"export", thinVG, tpl, "/var/lib/images/.img1.part"},
		"old file form, 4 args": {"export", thinVG, tpl, "/var/lib/images/.img1.part", "x"},
		"too few":               {"export", thinVG, tpl},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			e := newThinEnv(t)
			e.refuses(args...)
		})
	}
}

func TestThinSend(t *testing.T) {
	e := newThinEnv(t)
	r := e.run("send", thinVG, "vmexp-000000000001")
	if r.code != 0 || r.stderr != "" || r.stdout != "PLACEHOLDER-bytes\n" {
		t.Fatalf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	dev := "/dev/vg-test/vmexp-000000000001"
	e.want("chown", "vmtool:vmtool "+dev+"\nroot:disk "+dev+"\n")
	e.want("dd", "if="+dev+" bs=1M count=5368709120 iflag=count_bytes,fullblock status=none\n")
	e.want("setpriv", thinSetpriv+"dd if="+dev+" bs=1M count=5368709120 iflag=count_bytes,fullblock status=none\n")
	e.want("qemu-img", "")
}

func TestThinSendRestoresOwnerOnFailure(t *testing.T) {
	e := newThinEnv(t)
	addStub(t, e.stub, "dd", "exit 1")
	if r := e.run("send", thinVG, "vmexp-000000000001"); r.code == 0 {
		t.Fatal("exit 0 although dd failed")
	}
	dev := "/dev/vg-test/vmexp-000000000001"
	e.want("chown", "vmtool:vmtool "+dev+"\nroot:disk "+dev+"\n")
}

func TestThinSendRefusals(t *testing.T) {
	for _, n := range []string{
		"vmexp-000000000003", // export, no vmtool_bytes
		"vmexp-000000000004", // two vmtool_bytes
		"vmexp-000000000005", // longer than the LV
		"vmexp-000000000006", // not tagged vmtool_export
		"vmexp-000000000007", // a thin volume
		"vm1", "foreign", "thin-test", "vmtpl-000000000001", "nope",
	} {
		t.Run(n, func(t *testing.T) {
			e := newThinEnv(t)
			e.refuses("send", thinVG, n)
		})
	}
	e := newThinEnv(t)
	e.refuses("send", "vg-nopool", "vm1")
	e.refuses("send", thinVG)
	e.refuses("send", thinVG, "vmexp-000000000001", "1024")
}

// remove and tag take an export volume (and tag adds the checksum).
func TestThinExportVolumeRemoveAndTag(t *testing.T) {
	e := newThinEnv(t)
	e.ok("remove", thinVG, "vmexp-000000000001")
	e.want("lvremove", "-y vg-test/vmexp-000000000001\n")
	e.ok("tag", thinVG, "vmexp-000000000001", "+vmtool_sha256="+thinSum)
	e.want("lvchange", "--addtag vmtool_sha256="+thinSum+" vg-test/vmexp-000000000001\n")
}

// The other verbs still refuse an export volume, and tags cannot make or
// unmake one, or set its length.
func TestThinExportVolumeIsNotATargetOfOtherVerbs(t *testing.T) {
	e := newThinEnv(t)
	n := "vmexp-000000000001"
	e.refuses("snapshot", thinVG, n, "vm9", "vmtool_vol")
	e.refuses("extend", thinVG, n, "99999999999")
	e.refuses("rename", thinVG, n, "vmexp-000000000009")
	e.refuses("import", thinVG, n, e.img("img1"))
	e.refuses("create", thinVG, "thin-test", "vm9", "1024", "vmtool_vol", "vmtool_export")
	e.refuses("snapshot", thinVG, "vm1", "vm9", "vmtool_vol", "vmtool_export")
	e.refuses("tag", thinVG, n, "+vmtool_bytes=1")
	e.refuses("tag", thinVG, n, "-vmtool_bytes=5368709120")
	e.refuses("tag", thinVG, n, "-vmtool_export")
	e.refuses("tag", thinVG, "vm1", "+vmtool_export")
}

// Thick LVs without vmtool_export are refused by remove and tag.
func TestThinThickWithoutExportTagRefused(t *testing.T) {
	e := newThinEnv(t)
	for _, n := range []string{"vmexp-000000000006", "vmexp-000000000007", "foreign"} {
		e.refuses("remove", thinVG, n)
		e.refuses("tag", thinVG, n, "+vmtool_sha256="+thinSum)
	}
	if strings.Contains(e.rec("lvremove"), "vmexp") {
		t.Error("lvremove ran")
	}
}
