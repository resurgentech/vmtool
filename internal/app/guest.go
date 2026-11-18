package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// ExecVM runs argv, exactly, on VM ref over SSH and returns the result. Every
// argument reaches the guest as one argument; a shell only when argv is `sh -c`. stdin is written to the command's
// standard input, then closed; it is never logged or put in an error.
func (s *Service) ExecVM(_ context.Context, m Manager, ref VMRef, argv []string, stdin string) (*ExecResult, error) {
	if len(argv) == 0 {
		return nil, invalid("exec", fmt.Errorf("command is required"))
	}
	if err := validRef("exec", ref); err != nil {
		return nil, err
	}
	info, err := m.Info(ref.Name)
	if err != nil {
		return nil, wrap("exec", err)
	}
	if info.IP == "" {
		return nil, conflict("exec", fmt.Errorf("VM %q has no IP address", ref.String()))
	}
	c, err := vmtool.RemoteCmd(s.Tunnel, ref.Host, ref.Name, argv)
	if err != nil {
		return nil, wrap("exec", err)
	}
	c.Stdin = strings.NewReader(stdin)
	return runRemote("exec", c, 0)
}

func (s *Service) RunPlaybook(_ context.Context, m Manager, ref VMRef, playbook string, auth vmtool.Auth, extraVars map[string]string) (*PlaybookResult, error) {
	if err := vmtool.CheckExtraVars(extraVars); err != nil {
		return nil, invalid("playbook", err)
	}
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
	invPath, cleanup, err := vmtool.TempVMInventory(s.Tunnel, ref.Host, ref.Name, resolved, credsDir)
	if err != nil {
		return nil, wrap("playbook", err)
	}
	defer cleanup()
	out, err := vmtool.RunPlaybook(s.Tunnel, invPath, pb, withDeviceID(ref.Name, extraVars))
	res := &PlaybookResult{Output: out}
	if err != nil {
		return res, &Error{Kind: KindInternal, Op: "playbook", Err: err, Output: out}
	}
	return res, nil
}

// SSHCmd builds an interactive ssh command for VM ref through the tunnel. The
// caller sets nothing: stdio is wired. extraArgs follow the destination.
func (s *Service) SSHCmd(ref VMRef, extraArgs ...string) (*exec.Cmd, error) {
	if err := validRef("ssh", ref); err != nil {
		return nil, err
	}
	c, err := vmtool.SSHCmd(s.Tunnel, ref.Host, ref.Name, extraArgs...)
	if err != nil {
		return nil, wrap("ssh", err)
	}
	return c, nil
}

// RemoteCmd builds a non-interactive ssh command that runs argv, exactly (each
// argument quoted), on VM ref through the tunnel. Stdio is left unset for the caller.
func (s *Service) RemoteCmd(ref VMRef, argv []string) (*exec.Cmd, error) {
	if err := validRef("exec", ref); err != nil {
		return nil, err
	}
	c, err := vmtool.RemoteCmd(s.Tunnel, ref.Host, ref.Name, argv)
	if err != nil {
		return nil, wrap("exec", err)
	}
	return c, nil
}

// WriteInventory writes the persistent Ansible inventory for VM ref at path
// (host/vm, reached through the tunnel; no token is written). fallback is used
// when no credentials are stored for the VM. A missing parent directory skips
// the write.
func (s *Service) WriteInventory(ref VMRef, path string, fallback vmtool.Auth) error {
	if err := validRef("inventory", ref); err != nil {
		return err
	}
	credsDir, err := vmtool.AbsCredsDir(ref.Host, ref.Name)
	if err != nil {
		return wrap("inventory", err)
	}
	auth := vmtool.ResolveVMAuth(ref.Host, ref.Name, fallback)
	if _, err := os.Stat(filepath.Dir(path)); os.IsNotExist(err) {
		return nil
	}
	return wrap("inventory", vmtool.WriteVMInventory(path, s.Tunnel, ref.Host, ref.Name, auth, credsDir))
}
