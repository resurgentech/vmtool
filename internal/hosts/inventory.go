package hosts

import (
	"log/slog"
	"sync"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// InventorySource is what the daemon's reporter lists from: the local host's
// own libvirt, not the decorated Manager Resolve returns (the decorators hide
// the disk list), plus the thin helper's report to tell a vmtool thin volume
// from a plain block device.
func (r *Resolver) InventorySource() app.InventorySource { return inventorySource{r} }

type inventorySource struct{ r *Resolver }

// Open connects to the local libvirt for one build of the list.
func (s inventorySource) Open() (app.InventoryReader, func(), error) {
	m, done, err := s.r.local()
	if err != nil {
		return nil, nil, err
	}
	return &inventoryReader{m: m, thin: s.r.thin}, done, nil
}

// diskLister is the libvirt primitive the reporter needs; *vmtool.Manager has
// it. A Manager without it (a test fake) reports no disks.
type diskLister interface {
	VMDisks(name string) ([]vmtool.VMDisk, error)
}

type inventoryReader struct {
	m    app.Manager
	thin *vmtool.Thin

	once sync.Once
	vols []vmtool.ThinVolume
	ok   bool
}

func (i *inventoryReader) List() ([]vmtool.VMInfo, error) { return i.m.List() }

// thinVolumes reads the thin report at most once per build, and only when a
// block disk makes it matter. A host without thin pools (no helper, or one
// that fails) is logged once and its block disks stay block.
func (i *inventoryReader) thinVolumes() ([]vmtool.ThinVolume, bool) {
	i.once.Do(func() {
		if i.thin == nil {
			return
		}
		_, vols, err := i.thin.Report()
		if err != nil {
			slog.Warn("inventory: thin volumes not read; block disks are reported as block", "err", err)
			return
		}
		i.vols, i.ok = vols, true
	})
	return i.vols, i.ok
}

func (i *inventoryReader) VMDisks(name string) ([]app.InventoryDisk, error) {
	dl, ok := i.m.(diskLister)
	if !ok {
		return nil, nil
	}
	disks, err := dl.VMDisks(name)
	if err != nil {
		return nil, err
	}
	out := make([]app.InventoryDisk, 0, len(disks))
	for _, d := range disks {
		od := app.InventoryDisk{Target: d.Target, Pool: d.Pool, Kind: d.Kind, CapacityBytes: d.CapacityBytes}
		if d.Kind == vmtool.DiskBackingBlock {
			if vg, lv, isDev := vmtool.ParseThinDevPath(d.Path); isDev {
				if vols, have := i.thinVolumes(); have {
					if v, found := findVol(vols, vg, lv); found && v.Kind == vmtool.ThinKindVol {
						od.Kind, od.Pool, od.CapacityBytes = "thin", v.Pool, v.SizeBytes
					}
				}
			}
		}
		out = append(out, od)
	}
	return out, nil
}
