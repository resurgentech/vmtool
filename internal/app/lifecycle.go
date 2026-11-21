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

func (s *Service) DeleteVM(ctx context.Context, m Manager, ref VMRef, noclone bool) error {
	if err := validRef("delete", ref); err != nil {
		return err
	}
	if err := m.Delete(ref.Name, noclone); err != nil {
		return wrap("delete", err)
	}
	// noclone keeps the disk, so its guest users and keys are still valid.
	if !noclone {
		if err := s.forgetCreds(ctx, ref, s.noter(nil)); err != nil {
			return wrap("delete", err)
		}
	}
	return nil
}

func after(op string, m Manager, name string, err error) (*vmtool.VMInfo, error) {
	if err != nil {
		return nil, wrap(op, err)
	}
	info, ierr := m.Info(name)
	if ierr != nil {
		return nil, wrap(op, ierr)
	}
	return info, nil
}

func (s *Service) StartVM(_ context.Context, m Manager, name string) (*vmtool.VMInfo, error) {
	return after("start", m, name, m.Start(name))
}

func (s *Service) StopVM(_ context.Context, m Manager, name string) (*vmtool.VMInfo, error) {
	return after("stop", m, name, m.Stop(name))
}

func (s *Service) ResumeVM(_ context.Context, m Manager, name string) (*vmtool.VMInfo, error) {
	return after("resume", m, name, m.Resume(name))
}

func (s *Service) PoweroffVM(_ context.Context, m Manager, name string) (*vmtool.VMInfo, error) {
	return after("poweroff", m, name, m.Destroy(name))
}

func (s *Service) RebootVM(_ context.Context, m Manager, name string) (*vmtool.VMInfo, error) {
	return after("reboot", m, name, m.Reboot(name))
}

// SetComment sets a VM's comment, or clears it when comment is empty, and
// answers the VM. It is a metadata change: the VM is not redefined, and it
// works on a running and on a shut-off VM alike. A comment that breaks the
// rules (vmtool.ValidateComment) is KindInvalid and the host is not called.
func (s *Service) SetComment(_ context.Context, m Manager, name, comment string) (*vmtool.VMInfo, error) {
	if err := vmtool.ValidateComment(comment); err != nil {
		return nil, invalid("comment", err)
	}
	return after("comment", m, name, m.SetComment(name, comment))
}

func (s *Service) SetAutostart(_ context.Context, m Manager, name string, enabled bool) (*vmtool.VMInfo, error) {
	return after("autostart", m, name, m.SetAutostart(name, enabled))
}
