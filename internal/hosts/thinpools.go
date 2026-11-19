package hosts

import (
	"log/slog"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// ThinPools is a hypervisor daemon's local Manager with LVM thin pools added:
// ListPools lists, after the inner Manager's libvirt pools, every thin pool
// the root helper reports (see vmtool.Thin). Every other method passes through
// to the inner Manager unchanged; later steps override more of them here
// (clone into a thin pool, define a block disk, delete a thin volume).
//
// A helper that cannot run (no sudo rule, no helper installed, no LVM) is one
// log line per listing, not an error: the host just lists no thin pools.
// Resolve wraps a hypervisor-role local Manager only when the resolver has a
// Thin (WithThin; New sets one), so a workstation host never runs the helper,
// and it wraps inside hypervisorPools so an inner method sees the resolved
// pool name.
type ThinPools struct {
	app.Manager
	Thin *vmtool.Thin // the helper client; tests point its Helper at a stub sudo
	Log  *slog.Logger // nil means slog.Default()
}

func (t ThinPools) log() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default()
}

// ListPools is the inner Manager's pools followed by the thin pools. A thin
// pool whose name equals a listed pool's is left out and logged.
func (t ThinPools) ListPools() ([]vmtool.PoolInfo, error) {
	pools, err := t.Manager.ListPools()
	if err != nil {
		return nil, err
	}
	thin, _, err := t.Thin.Report()
	if err != nil {
		t.log().Warn("thin pools not listed", "err", err)
		return pools, nil
	}
	taken := make(map[string]bool, len(pools))
	for _, p := range pools {
		taken[p.Name] = true
	}
	for _, tp := range thin {
		if taken[tp.Name] {
			t.log().Warn("thin pool left out: a pool of that name exists", "pool", tp.Name, "vg", tp.VG)
			continue
		}
		taken[tp.Name] = true
		pools = append(pools, tp.PoolInfo())
	}
	return pools, nil
}

// RefreshPool keeps the optional interface the image cache uses visible
// through the wrapper.
func (t ThinPools) RefreshPool(pool string) error {
	if r, ok := t.Manager.(interface{ RefreshPool(string) error }); ok {
		return r.RefreshPool(pool)
	}
	return nil
}
