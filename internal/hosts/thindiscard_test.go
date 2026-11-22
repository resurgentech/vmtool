package hosts

import (
	"bytes"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// D1: when the helper keeps a volume because its discard failed, the line it
// printed (the volume, why it was kept, the retry command) reaches the error
// of `vmtool delete`, and a start-up building cleanup logs it.
const discardLine = "vmtool-thin: discard of vg-test/vm1 failed (Operation not supported); volume kept so its data does not return to the pool; retry: sudo -n /usr/local/libexec/vmtool-thin remove vg-test vm1\n"

func TestThinDeleteSurfacesTheHelperDiscardLine(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_vol")))
	h.write(t, "fail_remove", discardLine)
	inner := &thinInner{logFile: h.log, disks: []string{"/dev/vg-test/vm1"}}
	m := newThinFor(t, inner, newCache(t, uint64(gib), sha64a))
	err := m.Delete("vm1", false)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, w := range []string{"already deleted", "vg-test/vm1", "kept so its data does not return to the pool",
		"retry: sudo -n /usr/local/libexec/vmtool-thin remove vg-test vm1"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("err %q lacks %q", err, w)
		}
	}
}

func TestCleanBuildingLogsTheHelperDiscardLine(t *testing.T) {
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vmtbld-111111111111", gib, "vmtool_building")))
	h.write(t, "fail_remove", strings.ReplaceAll(discardLine, "vm1", "vmtbld-111111111111"))
	var buf bytes.Buffer
	if n := CleanThinBuilding(&vmtool.Thin{Helper: thinHelper}, logger(&buf)); n != 0 {
		t.Errorf("removed %d", n)
	}
	if !strings.Contains(buf.String(), "kept so its data does not return to the pool") || !strings.Contains(buf.String(), "vmtbld-111111111111") {
		t.Errorf("log = %q", buf.String())
	}
}
