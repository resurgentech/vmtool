package scripts_test

import (
	"path/filepath"
	"strings"
	"testing"
)

// Tests of D1: remove discards the volume's device before it removes it, and
// never removes one it could not discard. Same stubs as vmtool_thin_test.go;
// no real lvm or discard tool runs.

const thinHelperPath = "/usr/local/libexec/vmtool-thin"

// ordered makes the three tools that matter record, besides their own argv
// file, one shared log of "tool argv" lines, in call order.
func (e *thinEnv) ordered() {
	e.t.Helper()
	for _, tool := range []string{"lvchange", "blkdiscard", "lvremove"} {
		addStub(e.t, e.stub, tool, `echo "`+tool+` $*" >> "$d/calls.log"`)
	}
}

func (e *thinEnv) failDiscard(msg string) {
	e.t.Helper()
	body := `echo "blkdiscard $*" >> "$d/calls.log"; `
	if msg != "" {
		body += `echo '` + msg + `' >&2; `
	}
	addStub(e.t, e.stub, "blkdiscard", body+"exit 1")
}

func (e *thinEnv) calls() string { return readOr(e.t, filepath.Join(e.stub, "calls.log")) }

func (e *thinEnv) wantCalls(want string) {
	e.t.Helper()
	if got := e.calls(); got != want {
		e.t.Errorf("calls:\n got %q\nwant %q", got, want)
	}
}

// The volume row kinds for remove: active, inactive, open; thin and thick.
const thinDiscardListing = `  thin-test|twi-aotz--|64424509440|vmtool_thinpool
  act|Vwi-a-tz--|1073741824|vmtool_vol
  inact|Vwi---tz-k|1073741824|vmtool_vol
  inuse|Vwi-aotz--|1073741824|vmtool_vol
  vmtpl-000000000001|Vwi-a-tz--|1073741824|vmtool_template
  vmtbld-111111111111|Vwi---tz--|1073741824|vmtool_building
  vmexp-000000000001|-wi-a-----|1073741824|vmtool_export,vmtool_bytes=1024
  vmexp-000000000008|-wi-------|1073741824|vmtool_export,vmtool_bytes=1024
  vmexp-000000000009|-wi-ao----|1073741824|vmtool_export,vmtool_bytes=1024
`

func newDiscardEnv(t *testing.T) *thinEnv {
	e := newThinEnv(t)
	e.fixture(thinVG, thinDiscardListing)
	e.ordered()
	return e
}

// An active volume, a template and an export: the discard comes first and
// lvremove after it, on the volume's own device, with no other option.
func TestThinRemoveDiscardsFirst(t *testing.T) {
	for _, n := range []string{"act", "vmtpl-000000000001", "vmexp-000000000001"} {
		t.Run(n, func(t *testing.T) {
			e := newDiscardEnv(t)
			e.ok("remove", thinVG, n)
			e.wantCalls("blkdiscard /dev/vg-test/" + n + "\nlvremove -y vg-test/" + n + "\n")
			e.want("blkdiscard", "/dev/vg-test/"+n+"\n")
		})
	}
}

// An inactive volume is activated with -K (thin volumes may carry the
// activation-skip flag) before the discard.
func TestThinRemoveActivatesInactiveFirst(t *testing.T) {
	for _, n := range []string{"inact", "vmtbld-111111111111", "vmexp-000000000008"} {
		t.Run(n, func(t *testing.T) {
			e := newDiscardEnv(t)
			e.ok("remove", thinVG, n)
			e.wantCalls("lvchange -ay -K vg-test/" + n + "\nblkdiscard /dev/vg-test/" + n + "\nlvremove -y vg-test/" + n + "\n")
		})
	}
}

// A volume LVM reports as open is refused: no activation, no discard, no
// removal.
func TestThinRemoveRefusesOpenVolume(t *testing.T) {
	for _, n := range []string{"inuse", "vmexp-000000000009"} {
		t.Run(n, func(t *testing.T) {
			e := newDiscardEnv(t)
			e.refuses("remove", thinVG, n)
			if got := e.calls(); got != "" {
				t.Errorf("calls %q", got)
			}
			r := e.run("remove", thinVG, n)
			if !strings.Contains(r.stderr, n) || !strings.Contains(r.stderr, "open") {
				t.Errorf("stderr %q", r.stderr)
			}
		})
	}
}

// A failed discard: no lvremove, one line that names the volume, says it was
// kept, and gives the exact retry command.
func TestThinRemoveDiscardFailureKeepsVolume(t *testing.T) {
	for _, n := range []string{"act", "vmexp-000000000001"} {
		t.Run(n, func(t *testing.T) {
			e := newDiscardEnv(t)
			e.failDiscard("blkdiscard: /dev/vg-test/" + n + ": BLKDISCARD ioctl failed: Operation not supported")
			r := e.run("remove", thinVG, n)
			if r.code == 0 {
				t.Fatal("exit 0 although the discard failed")
			}
			e.wantCalls("blkdiscard /dev/vg-test/" + n + "\n")
			if got := e.rec("lvremove"); got != "" {
				t.Errorf("lvremove ran: %q", got)
			}
			line := strings.TrimSuffix(r.stderr, "\n")
			if strings.Contains(line, "\n") || !strings.HasPrefix(line, "vmtool-thin: ") || r.stdout != "" {
				t.Fatalf("want one prefixed stderr line, got %q (stdout %q)", r.stderr, r.stdout)
			}
			for _, w := range []string{"vg-test/" + n, "kept", "does not return to the pool", "Operation not supported",
				"retry: sudo -n " + thinHelperPath + " remove vg-test " + n} {
				if !strings.Contains(line, w) {
					t.Errorf("line %q lacks %q", line, w)
				}
			}
		})
	}
	// A discard that says nothing is still a refusal that names the volume.
	e := newDiscardEnv(t)
	e.failDiscard("")
	r := e.run("remove", thinVG, "act")
	if r.code == 0 || !strings.Contains(r.stderr, "vg-test/act") || strings.Count(r.stderr, "\n") != 1 {
		t.Errorf("exit %d stderr %q", r.code, r.stderr)
	}
	e.want("lvremove", "")
}

// If activation fails nothing is discarded or removed.
func TestThinRemoveActivationFailure(t *testing.T) {
	e := newDiscardEnv(t)
	addStub(t, e.stub, "lvchange", `echo "lvchange $*" >> "$d/calls.log"; exit 5`)
	if r := e.run("remove", thinVG, "inact"); r.code == 0 {
		t.Fatal("exit 0 although activation failed")
	}
	e.wantCalls("lvchange -ay -K vg-test/inact\n")
}

// The export trap discards the half-made volume, then removes it; the node's
// owner is given back first. A volume the discard cannot clear stays.
func TestThinExportTrapDiscardsBeforeRemoving(t *testing.T) {
	e := newThinEnv(t)
	e.ordered()
	addStub(t, e.stub, "qemu-img", `[[ $1 == convert ]] && exit 3; `+thinQemuBody)
	if r := e.run(thinExportArgs...); r.code == 0 {
		t.Fatal("exit 0 although convert failed")
	}
	e.wantCalls("blkdiscard " + thinExpDev + "\nlvremove -y vg-test/" + thinExpNew + "\n")
	e.want("chown", thinBothBack)
}

func TestThinExportTrapDiscardFailureKeepsVolume(t *testing.T) {
	e := newThinEnv(t)
	e.ordered()
	e.failDiscard("blkdiscard: busy")
	addStub(t, e.stub, "qemu-img", `[[ $1 == convert ]] && exit 3; `+thinQemuBody)
	r := e.run(thinExportArgs...)
	if r.code == 0 {
		t.Fatal("exit 0 although convert failed")
	}
	e.wantCalls("blkdiscard " + thinExpDev + "\n")
	if got := e.rec("lvremove"); got != "" {
		t.Errorf("lvremove ran: %q", got)
	}
	e.want("chown", thinBothBack)
	for _, w := range []string{"vg-test/" + thinExpNew, "kept", "retry: sudo -n " + thinHelperPath + " remove vg-test " + thinExpNew} {
		if !strings.Contains(r.stderr, w) {
			t.Errorf("stderr %q lacks %q", r.stderr, w)
		}
	}
}

// A good export, and a failure before the volume exists, discard nothing.
func TestThinExportDiscardsNothingWhenNothingToRemove(t *testing.T) {
	e := newThinEnv(t)
	e.ordered()
	if r := e.run(thinExportArgs...); r.code != 0 {
		t.Fatalf("exit %d: %q", r.code, r.stderr)
	}
	e.want("blkdiscard", "")
	e.want("lvremove", "")

	e = newThinEnv(t)
	addStub(t, e.stub, "lvcreate", "exit 5")
	if r := e.run(thinExportArgs...); r.code == 0 {
		t.Fatal("exit 0 although lvcreate failed")
	}
	e.want("blkdiscard", "")
}

// No verb but remove discards: nothing else calls blkdiscard.
func TestThinOtherVerbsNeverDiscard(t *testing.T) {
	e := newDiscardEnv(t)
	e.ok("create", thinVG, "thin-test", "vm9", "1024", "vmtool_vol")
	e.ok("snapshot", thinVG, "vmtpl-000000000001", "vm9", "vmtool_vol")
	e.ok("extend", thinVG, "act", "2147483648")
	e.ok("rename", thinVG, "vmtbld-111111111111", "vmtpl-000000000009")
	e.ok("tag", thinVG, "act", "+vmtool_x")
	e.ok("import", thinVG, "vmtbld-111111111111", e.img("img1"))
	e.run("send", thinVG, "vmexp-000000000001")
	e.want("blkdiscard", "")
}
