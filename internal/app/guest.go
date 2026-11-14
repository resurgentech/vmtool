package app

import (
	"context"
	"fmt"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

func (s *Service) RunCommand(_ context.Context, m Manager, ref VMRef, argv []string) (*CmdResult, error) {
	if len(argv) == 0 {
		return nil, invalid("cmd", fmt.Errorf("command is required"))
	}
	if err := validRef("cmd", ref); err != nil {
		return nil, err
	}
	info, err := m.Info(ref.Name)
	if err != nil {
		return nil, wrap("cmd", err)
	}
	if info.IP == "" {
		return nil, conflict("cmd", fmt.Errorf("VM %q has no IP address", ref.String()))
	}
	c, err := vmtool.RemoteCmd(ref.Host, ref.Name, info.IP, argv)
	if err != nil {
		return nil, wrap("cmd", err)
	}
	return runRemote("cmd", c, 0)
}

func (s *Service) RunPlaybook(_ context.Context, m Manager, ref VMRef, playbook string, auth vmtool.Auth, extraVars map[string]string) (*PlaybookResult, error) {
	pb, err := s.resolvePlaybook(playbook)
	if err != nil {
		return nil, err
	}
	if err := validRef("playbook", ref); err != nil {
		return nil, err
	}
	info, err := m.Info(ref.Name)
	if err != nil {
		return nil, wrap("playbook", err)
	}
	if info.IP == "" {
		return nil, conflict("playbook", fmt.Errorf("VM %q has no IP address", ref.String()))
	}
	credsDir, err := vmtool.AbsCredsDir(ref.Host, ref.Name)
	if err != nil {
		return nil, wrap("playbook", err)
	}
	resolved := vmtool.ResolveVMAuth(ref.Host, ref.Name, auth)
	invPath, cleanup, err := vmtool.TempInventory(ref.Name, info.IP, resolved, credsDir)
	if err != nil {
		return nil, wrap("playbook", err)
	}
	defer cleanup()
	out, err := vmtool.RunPlaybook(invPath, pb, withDeviceID(ref.Name, extraVars))
	res := &PlaybookResult{Output: out}
	if err != nil {
		return res, &Error{Kind: KindInternal, Op: "playbook", Err: err, Output: out}
	}
	return res, nil
}
