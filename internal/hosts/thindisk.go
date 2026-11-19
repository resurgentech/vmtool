package hosts

import (
	"errors"
	"fmt"
	"math"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Add-disk and resize-disk on thin pools: ThinPools' overrides. A VM whose
// pool or boot disk is not a vmtool thin volume goes to the inner Manager
// exactly as before (so a thick block vda keeps its refusal and a qcow2 disk
// its resize). Only a thin volume this layer can see in the helper's report is
// handled here.
//
// The libvirt side is three small primitives on *vmtool.Manager (DomainDisks,
// AttachBlockDisk, BlockResizeBoot), found through thinDomain: an inner
// Manager without them cannot do thin disks, and is treated as "not thin".

type thinDomain interface {
	DomainDisks(name string) (*vmtool.DomainDisks, error)
	AttachBlockDisk(name, path string) (*vmtool.DiskDevice, error)
	BlockResizeBoot(name string, sizeBytes uint64) error
}

func noRoomOp(op string, err error) error {
	if errors.Is(err, vmtool.ErrThinNoRoom) {
		return &app.Error{Kind: app.KindConflict, Op: op, Err: err}
	}
	return err
}

const gibBytes = uint64(1 << 30)

// AddDisk into a thin pool: the room check, then a new thin volume <vm>-N
// tagged vmtool_vol, then its attach as a block disk. A failed attach removes
// exactly the volume this call made. Any other pool is the inner Manager's.
func (t ThinPools) AddDisk(name string, sizeGB uint, pool string) (*vmtool.DiskDevice, error) {
	tp, vols, ok := t.thinTarget(pool)
	dm, hasDom := t.Manager.(thinDomain)
	if !ok || !hasDom {
		return t.Manager.AddDisk(name, sizeGB, pool)
	}
	if sizeGB == 0 {
		return nil, fmt.Errorf("size must be greater than 0")
	}
	dom, err := dm.DomainDisks(name)
	if err != nil {
		return nil, err
	}
	lv := vmtool.ExtraLVName(name, dom.Used)
	if !vmtool.ValidThinLVName(lv) {
		return nil, invalidErr("add disk", "%q is not usable as a thin volume name", lv)
	}
	if _, exists := findVol(vols, tp.VG, lv); exists {
		return nil, invalidErr("add disk", "the thin volume %s/%s already exists", tp.VG, lv)
	}
	size := uint64(sizeGB) * gibBytes
	if size > math.MaxInt64 {
		return nil, invalidErr("add disk", "size %d GB is too large", sizeGB)
	}
	if err := vmtool.CheckThinRoom(tp, int64(size)); err != nil {
		return nil, noRoomOp("add_disk", err)
	}
	if err := t.Thin.Create(tp.VG, tp.Name, lv, int64(size), vmtool.ThinTagVol); err != nil {
		return nil, err // nothing of ours exists: remove nothing
	}
	disk, err := dm.AttachBlockDisk(name, vmtool.ThinDevPath(tp.VG, lv))
	if err != nil {
		// lv is the volume this call has just made.
		if rerr := t.Thin.Remove(tp.VG, lv); rerr != nil {
			return nil, fmt.Errorf("%w (and removing the new volume %s/%s failed: %v)", err, tp.VG, lv, rerr)
		}
		return nil, err
	}
	return disk, nil
}

// thinBoot finds the boot disk's thin volume and its pool in the report, when
// vda is a vmtool_vol of a vmtool thin pool.
func (t ThinPools) thinBoot(dom *vmtool.DomainDisks) (vmtool.ThinPool, vmtool.ThinVolume, bool) {
	if t.Thin == nil || dom.BootKind != vmtool.DiskBackingBlock {
		return vmtool.ThinPool{}, vmtool.ThinVolume{}, false
	}
	vg, lv, ok := vmtool.ParseThinDevPath(dom.BootPath)
	if !ok {
		return vmtool.ThinPool{}, vmtool.ThinVolume{}, false
	}
	pools, vols, err := t.Thin.Report()
	if err != nil {
		t.log().Warn("thin volumes not read", "err", err)
		return vmtool.ThinPool{}, vmtool.ThinVolume{}, false
	}
	v, found := findVol(vols, vg, lv)
	if !found || v.Kind != vmtool.ThinKindVol {
		return vmtool.ThinPool{}, vmtool.ThinVolume{}, false
	}
	for _, p := range pools {
		if p.VG == v.VG && p.Name == v.Pool {
			return p, v, true
		}
	}
	return vmtool.ThinPool{}, vmtool.ThinVolume{}, false
}

// ResizeDisk grows a thin vda: refuse a size that is not larger, check the
// room for the growth, extend the volume, and tell a live domain. Any other
// boot disk is the inner Manager's.
func (t ThinPools) ResizeDisk(name string, sizeGB uint) error {
	dm, hasDom := t.Manager.(thinDomain)
	if !hasDom || sizeGB == 0 {
		return t.Manager.ResizeDisk(name, sizeGB)
	}
	dom, err := dm.DomainDisks(name)
	if err != nil {
		return t.Manager.ResizeDisk(name, sizeGB) // the inner Manager reports it
	}
	pool, v, ok := t.thinBoot(dom)
	if !ok {
		return t.Manager.ResizeDisk(name, sizeGB)
	}
	want := uint64(sizeGB) * gibBytes
	if want > math.MaxInt64 {
		return invalidErr("resize disk", "size %d GB is too large", sizeGB)
	}
	if want <= uint64(v.SizeBytes) {
		return invalidErr("resize disk", "new size %dGB must be larger than current %dGB", sizeGB, uint64(v.SizeBytes)/gibBytes)
	}
	if err := t.growThin(pool, v, int64(want)); err != nil {
		return noRoomOp("resize_disk", err)
	}
	if dom.Running {
		if err := dm.BlockResizeBoot(name, want); err != nil {
			return fmt.Errorf("thin volume %s/%s is already extended to %d bytes, but the running VM was not told: %w; it is not shrunk, and the next resize or a reboot picks up the new size", v.VG, v.Name, want, err)
		}
	}
	return nil
}

// growThin checks the room for the growth (not the new size: the volume's
// current size is already among the promised bytes) and extends the volume.
func (t ThinPools) growThin(pool vmtool.ThinPool, v vmtool.ThinVolume, want int64) error {
	if err := vmtool.CheckThinRoom(pool, want-v.SizeBytes); err != nil {
		return err
	}
	return t.Thin.Extend(v.VG, v.Name, want)
}
