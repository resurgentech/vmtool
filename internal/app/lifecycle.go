package app

import (
	"context"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

func (s *Service) GetVM(_ context.Context, m Manager, name string) (*vmtool.VMInfo, error) {
	info, err := m.Info(name)
	if err != nil {
		return nil, wrap("get", err)
	}
	return info, nil
}

func (s *Service) ListVMs(_ context.Context, m Manager) ([]vmtool.VMInfo, error) {
	vms, err := m.List()
	if err != nil {
		return nil, wrap("list", err)
	}
	if vms == nil {
		vms = []vmtool.VMInfo{}
	}
	return vms, nil
}

// DeleteVM deletes VM ref on its host. In store mode it also removes the
// VM's inventory row (and, unless noclone, its credential record), so a VM
// deleted on purpose does not wait out the delist grace period. The row is
// named by the VM's UUID, read before the VM is gone; a host that reports none
// (older than 2.3.0) leaves the row to its host's next report. With noclone
// the definition is gone all the same, so the row goes too; the credentials
// stay with the disk.
func (s *Service) DeleteVM(ctx context.Context, m Manager, ref VMRef, noclone bool) error {
	if err := validRef("delete", ref); err != nil {
		return err
	}
	uuid := ""
	if s.storeMode(ctx) {
		if info, err := m.Info(ref.Name); err == nil {
			uuid = info.UUID
		}
	}
	if err := m.Delete(ref.Name, noclone); err != nil {
		return s.missingVM(ctx, m, ref, wrap("delete", err))
	}
	// One line for a store that is down, not one per call.
	var said bool
	note := func(line string) {
		said = true
		s.noter(nil)(line)
	}
	// noclone keeps the disk, so its guest users and keys are still valid.
	var cerr error
	if !noclone {
		cerr = s.forgetCreds(ctx, ref, note)
	}
	if !said {
		s.dropInventoryRow(ctx, ref, uuid, note)
	} else {
		s.dropInventoryRow(ctx, ref, uuid, nil)
	}
	if cerr != nil {
		return wrap("delete", cerr)
	}
	return nil
}

// after answers the VM's Info once op's call err is nil. Every error, whether
// from the call or from Info, goes through missingVM, so a VM its host does not
// have is delisted from the store's inventory (store mode only).
func (s *Service) after(ctx context.Context, op string, m Manager, ref VMRef, err error) (*vmtool.VMInfo, error) {
	if err != nil {
		return nil, s.missingVM(ctx, m, ref, wrap(op, err))
	}
	info, ierr := m.Info(ref.Name)
	if ierr != nil {
		return nil, s.missingVM(ctx, m, ref, wrap(op, ierr))
	}
	return info, nil
}

func (s *Service) StartVM(ctx context.Context, m Manager, ref VMRef) (*vmtool.VMInfo, error) {
	return s.after(ctx, "start", m, ref, m.Start(ref.Name))
}

func (s *Service) StopVM(ctx context.Context, m Manager, ref VMRef) (*vmtool.VMInfo, error) {
	return s.after(ctx, "stop", m, ref, m.Stop(ref.Name))
}

func (s *Service) ResumeVM(ctx context.Context, m Manager, ref VMRef) (*vmtool.VMInfo, error) {
	return s.after(ctx, "resume", m, ref, m.Resume(ref.Name))
}

func (s *Service) PoweroffVM(ctx context.Context, m Manager, ref VMRef) (*vmtool.VMInfo, error) {
	return s.after(ctx, "poweroff", m, ref, m.Destroy(ref.Name))
}

func (s *Service) RebootVM(ctx context.Context, m Manager, ref VMRef) (*vmtool.VMInfo, error) {
	return s.after(ctx, "reboot", m, ref, m.Reboot(ref.Name))
}

func (s *Service) SetAutostart(ctx context.Context, m Manager, ref VMRef, enabled bool) (*vmtool.VMInfo, error) {
	return s.after(ctx, "autostart", m, ref, m.SetAutostart(ref.Name, enabled))
}
