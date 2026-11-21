package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

func (s *Service) MigrateDisk(ctx context.Context, m Manager, ref VMRef, pool string) (*vmtool.VMInfo, error) {
	if pool == "" {
		return nil, invalid("migrate_disk", fmt.Errorf("pool is required"))
	}
	if err := m.MigrateDisk(ref.Name, pool); err != nil {
		return nil, s.missingVM(ctx, m, ref, wrap("migrate_disk", err))
	}
	info, err := m.Info(ref.Name)
	if err != nil {
		return nil, s.missingVM(ctx, m, ref, wrap("migrate_disk", err))
	}
	return info, nil
}

func (s *Service) ResizeDisk(ctx context.Context, m Manager, ref VMRef, sizeGB uint, auth vmtool.Auth) (*ResizeResult, error) {
	if sizeGB == 0 {
		return nil, invalid("resize_disk", fmt.Errorf("size must be > 0"))
	}
	if err := validRef("resize_disk", ref); err != nil {
		return nil, err
	}
	if err := m.ResizeDisk(ref.Name, sizeGB); err != nil {
		return nil, s.missingVM(ctx, m, ref, wrap("resize_disk", err))
	}
	info, err := m.Info(ref.Name)
	if err != nil {
		return nil, s.missingVM(ctx, m, ref, wrap("resize_disk", err))
	}
	res := &ResizeResult{VM: *info}
	if info.State != vmtool.StateRunning || info.IP == "" {
		return res, nil
	}
	credsDir, err := vmtool.AbsCredsDir(ref.Host, ref.Name)
	if err != nil {
		res.GrowError = err.Error()
		return res, nil
	}
	s.prepareCreds(ctx, ref, info.UUID, s.noter(nil))
	resolved := vmtool.ResolveVMAuth(ref.Host, ref.Name, auth)
	invPath, cleanup, err := vmtool.TempVMInventory(s.Tunnel, ref.Host, ref.Name, resolved, credsDir)
	if err != nil {
		res.GrowError = err.Error()
		return res, nil
	}
	defer cleanup()
	// No wait_ssh here: the VM is already running with an IP, so sshd is
	// normally up, and a wait would delay the failure report by up to 120s.
	out, err := vmtool.GrowDisk(s.Tunnel, invPath) // once: no blind retry
	if err != nil {
		res.GrowError = err.Error()
		if out = strings.TrimSpace(out); out != "" {
			res.GrowError += "\n" + out
		}
		return res, nil
	}
	res.FilesystemGrown = true
	return res, nil
}

func (s *Service) AddDisk(ctx context.Context, m Manager, ref VMRef, sizeGB uint, pool string) (*AddDiskResult, error) {
	if sizeGB == 0 {
		return nil, invalid("add_disk", fmt.Errorf("size must be > 0"))
	}
	disk, err := m.AddDisk(ref.Name, sizeGB, pool)
	if err != nil {
		return nil, s.missingVM(ctx, m, ref, wrap("add_disk", err))
	}
	info, err := m.Info(ref.Name)
	if err != nil {
		return nil, s.missingVM(ctx, m, ref, wrap("add_disk", err))
	}
	return &AddDiskResult{VM: *info, Path: disk.Path, Target: disk.Target}, nil
}
