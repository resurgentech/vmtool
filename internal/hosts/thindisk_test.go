package hosts

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// diskInner is thinInner plus the libvirt primitives the thin disk operations
// use, with the domain it answers for.
type diskInner struct {
	*thinInner
	dom       vmtool.DomainDisks
	attachErr error
	resizeErr error
	domErr    error
}

func (f *diskInner) DomainDisks(name string) (*vmtool.DomainDisks, error) {
	f.rec("DomainDisks %s", name)
	if f.domErr != nil {
		return nil, f.domErr
	}
	d := f.dom
	return &d, nil
}
func (f *diskInner) AttachBlockDisk(name, path string) (*vmtool.DiskDevice, error) {
	f.rec("AttachBlockDisk %s %s", name, path)
	if f.attachErr != nil {
		return nil, f.attachErr
	}
	return &vmtool.DiskDevice{Path: path, Target: "vdc", Kind: vmtool.DiskBackingBlock}, nil
}
func (f *diskInner) BlockResizeBoot(name string, b uint64) error {
	f.rec("BlockResizeBoot %s %d", name, b)
	return f.resizeErr
}
func (f *diskInner) AddDisk(name string, gb uint, pool string) (*vmtool.DiskDevice, error) {
	f.rec("AddDisk %s %d %s", name, gb, pool)
	return &vmtool.DiskDevice{Path: "/var/lib/vmtool/images/x.qcow2", Target: "vdb"}, nil
}
func (f *diskInner) ResizeDisk(name string, gb uint) error {
	f.rec("ResizeDisk %s %d", name, gb)
	return nil
}

func (f *diskInner) only(prefix string) []string {
	var out []string
	for _, e := range f.events {
		if strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func thinDom(running bool) vmtool.DomainDisks {
	return vmtool.DomainDisks{BootPath: "/dev/vg-test/vm1", BootKind: vmtool.DiskBackingBlock,
		Used: []string{"vda", "vdb"}, Running: running, VCPUs: 2}
}

// vm1 (20 GiB) and vm1-2 (10 GiB) in the pool at the given data percent.
func vmReport(data string) string {
	return reportOf(poolRow(data), thinLV("vm1", 20*gib, "vmtool_vol"), thinLV("vm1-2", 10*gib, "vmtool_vol"))
}

func newDiskThin(t *testing.T, data string, dom vmtool.DomainDisks) (*helperStub, *diskInner, ThinPools) {
	t.Helper()
	h := newHelperStub(t)
	h.report(t, vmReport(data))
	inner := &diskInner{thinInner: &thinInner{}, dom: dom}
	return h, inner, newThinFor(t, inner, cache{})
}

func TestThinAddDisk(t *testing.T) {
	h, inner, m := newDiskThin(t, "25.00", thinDom(true))
	d, err := m.AddDisk("vm1", 5, "thin-test")
	if err != nil || d.Path != "/dev/vg-test/vm1-3" {
		t.Fatalf("AddDisk = %+v, %v", d, err)
	}
	eqStrings(t, "helper argv", h.calls(t), []string{"report", "create vg-test thin-test vm1-3 5368709120 vmtool_vol"})
	eqStrings(t, "attach", inner.only("AttachBlockDisk"), []string{"AttachBlockDisk vm1 /dev/vg-test/vm1-3"})
	if len(inner.only("AddDisk")) != 0 {
		t.Error("the libvirt AddDisk ran for a thin pool")
	}
}

func TestThinAddDiskNoRoom(t *testing.T) {
	h, inner, m := newDiskThin(t, "85.00", thinDom(true))
	_, err := m.AddDisk("vm1", 5, "thin-test")
	var ae *app.Error
	if !errors.Is(err, vmtool.ErrThinNoRoom) || !errors.As(err, &ae) || ae.Kind != app.KindConflict {
		t.Fatalf("err = %v, want a conflict matching ErrThinNoRoom", err)
	}
	if w := h.writes(t); len(w) != 0 {
		t.Errorf("helper writes: %q", w)
	}
	if len(inner.only("AttachBlockDisk")) != 0 {
		t.Error("attached despite no room")
	}
}

func TestThinAddDiskAttachFails(t *testing.T) {
	h, inner, m := newDiskThin(t, "25.00", thinDom(true))
	inner.attachErr = fmt.Errorf("attaching disk: boom")
	_, err := m.AddDisk("vm1", 5, "thin-test")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	eqStrings(t, "writes", h.writes(t), []string{
		"create vg-test thin-test vm1-3 5368709120 vmtool_vol",
		"remove vg-test vm1-3",
	})
}

func TestThinAddDiskElsewhereIsInner(t *testing.T) {
	h, inner, m := newDiskThin(t, "25.00", thinDom(true))
	d, err := m.AddDisk("vm1", 5, "vmtool")
	if err != nil || d.Path != "/var/lib/vmtool/images/x.qcow2" {
		t.Fatalf("AddDisk = %+v, %v", d, err)
	}
	eqStrings(t, "inner", inner.only("AddDisk"), []string{"AddDisk vm1 5 vmtool"})
	if w := h.writes(t); len(w) != 0 {
		t.Errorf("helper writes: %q", w)
	}
}

func TestThinResizeDiskShutOff(t *testing.T) {
	h, inner, m := newDiskThin(t, "25.00", thinDom(false))
	if err := m.ResizeDisk("vm1", 30); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "writes", h.writes(t), []string{"extend vg-test vm1 32212254720"})
	if len(inner.only("BlockResizeBoot")) != 0 || len(inner.only("ResizeDisk")) != 0 {
		t.Errorf("inner called: %q", inner.events)
	}
}

func TestThinResizeDiskRunning(t *testing.T) {
	h, inner, m := newDiskThin(t, "25.00", thinDom(true))
	if err := m.ResizeDisk("vm1", 30); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "writes", h.writes(t), []string{"extend vg-test vm1 32212254720"})
	eqStrings(t, "blockresize", inner.only("BlockResizeBoot"), []string{"BlockResizeBoot vm1 32212254720"})
}

func TestThinResizeDiskBlockResizeFails(t *testing.T) {
	h, inner, m := newDiskThin(t, "25.00", thinDom(true))
	inner.resizeErr = fmt.Errorf("qemu said no")
	err := m.ResizeDisk("vm1", 30)
	if err == nil || !strings.Contains(err.Error(), "already extended") || !strings.Contains(err.Error(), "qemu said no") {
		t.Fatalf("err = %v", err)
	}
	eqStrings(t, "writes", h.writes(t), []string{"extend vg-test vm1 32212254720"}) // never shrunk
}

func TestThinResizeDiskSmallerRefused(t *testing.T) {
	for _, gb := range []uint{20, 10} {
		h, inner, m := newDiskThin(t, "25.00", thinDom(true))
		err := m.ResizeDisk("vm1", gb)
		want := fmt.Sprintf("new size %dGB must be larger than current 20GB", gb)
		var ae *app.Error
		if err == nil || !strings.Contains(err.Error(), want) || !errors.As(err, &ae) || ae.Kind != app.KindInvalid {
			t.Errorf("%dGB: err = %v", gb, err)
		}
		if w := h.writes(t); len(w) != 0 {
			t.Errorf("%dGB: helper writes %q", gb, w)
		}
		if len(inner.only("BlockResizeBoot")) != 0 {
			t.Errorf("%dGB: BlockResize called", gb)
		}
	}
}

func TestThinResizeDiskNoRoom(t *testing.T) {
	h, inner, m := newDiskThin(t, "85.00", thinDom(true))
	err := m.ResizeDisk("vm1", 30)
	var ae *app.Error
	if !errors.Is(err, vmtool.ErrThinNoRoom) || !errors.As(err, &ae) || ae.Kind != app.KindConflict {
		t.Fatalf("err = %v", err)
	}
	if w := h.writes(t); len(w) != 0 {
		t.Errorf("helper writes: %q", w)
	}
	if len(inner.only("BlockResizeBoot")) != 0 {
		t.Error("BlockResize called")
	}
}

// Room is checked for the growth, not the new size: 60 GiB pool x4 = 240 GiB;
// virtual is 30 (20+10); growing vm1 by 200 GiB fits (230) though the new
// size alone plus the virtual sum would not.
func TestThinResizeDiskRoomIsGrowth(t *testing.T) {
	h, _, m := newDiskThin(t, "25.00", thinDom(false))
	if err := m.ResizeDisk("vm1", 220); err != nil {
		t.Fatalf("growth of 200 GiB with 30 GiB promised in a 240 GiB limit: %v", err)
	}
	eqStrings(t, "writes", h.writes(t), []string{"extend vg-test vm1 236223201280"})
	if err := m.ResizeDisk("vm1", 221); err != nil { // vm1 is still 20 GiB in the report
		t.Logf("a second, larger growth is refused: %v", err)
	}
}

func TestThinResizeDiskNotThinIsInner(t *testing.T) {
	// A thick block vda and a qcow2 vda are not thin volumes: the inner
	// Manager's ResizeDisk decides (it keeps the block-grow refusal).
	for _, boot := range []vmtool.DomainDisks{
		{BootPath: "/dev/vg-thick/vm1", BootKind: vmtool.DiskBackingBlock, Used: []string{"vda"}, Running: true},
		{BootPath: "/var/lib/vmtool/images/vm1.qcow2", BootKind: vmtool.DiskBackingFile, Used: []string{"vda"}, Running: true},
	} {
		h, inner, m := newDiskThin(t, "25.00", boot)
		if err := m.ResizeDisk("vm1", 30); err != nil {
			t.Fatal(err)
		}
		eqStrings(t, "inner", inner.only("ResizeDisk"), []string{"ResizeDisk vm1 30"})
		if w := h.writes(t); len(w) != 0 {
			t.Errorf("helper writes: %q", w)
		}
	}
}

func TestThinResizeDiskForeignLVIsInner(t *testing.T) {
	// vda is /dev/vg-test/vm1 but the report has it as a template: not ours.
	h := newHelperStub(t)
	h.report(t, reportOf(poolRow("25.00"), thinLV("vm1", 20*gib, "vmtool_template")))
	inner := &diskInner{thinInner: &thinInner{}, dom: thinDom(true)}
	m := newThinFor(t, inner, cache{})
	if err := m.ResizeDisk("vm1", 30); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "inner", inner.only("ResizeDisk"), []string{"ResizeDisk vm1 30"})
	if w := h.writes(t); len(w) != 0 {
		t.Errorf("helper writes: %q", w)
	}
}

func TestThinResizeVolumeGrows(t *testing.T) {
	h, inner, m := newDiskThin(t, "25.00", thinDom(false))
	if err := m.ResizeVolume("/dev/vg-test/vm1", uint64(30*gib)); err != nil {
		t.Fatal(err)
	}
	eqStrings(t, "writes", h.writes(t), []string{"extend vg-test vm1 32212254720"})
	if len(inner.only("ResizeVolume")) != 0 {
		t.Error("inner ResizeVolume ran")
	}
	// no room: refused before extend
	h2, _, m2 := newDiskThin(t, "85.00", thinDom(false))
	if err := m2.ResizeVolume("/dev/vg-test/vm1", uint64(30*gib)); !errors.Is(err, vmtool.ErrThinNoRoom) {
		t.Errorf("err = %v", err)
	}
	if w := h2.writes(t); len(w) != 0 {
		t.Errorf("writes %q", w)
	}
}

// create's extra disk into a thin pool stays refused (T5); add-disk is the way.
func TestThinCreateVolumeStillRefused(t *testing.T) {
	h, inner, m := newDiskThin(t, "25.00", thinDom(false))
	_, err := m.CreateVolume("thin-test", "vm9-2.qcow2", 5)
	var ae *app.Error
	if !errors.As(err, &ae) || ae.Kind != app.KindInvalid {
		t.Fatalf("err = %v", err)
	}
	if w := h.writes(t); len(w) != 0 || len(inner.only("CreateVolume")) != 0 {
		t.Errorf("something ran: %q %q", w, inner.events)
	}
}
