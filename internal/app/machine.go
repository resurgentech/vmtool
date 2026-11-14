package app

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

type MachineState string

const (
	MachineReachable   MachineState = "reachable"
	MachineUnreachable MachineState = "unreachable"
	MachineRebooting   MachineState = "rebooting"
)

// MachineInfo is a registered metal/alien machine plus a live SSH probe.
type MachineInfo struct {
	vmtool.Machine
	State MachineState
	OS    string
	// Error is the probe failure when State is unreachable.
	Error string
}

type AddMachineInput struct {
	Name       string
	Host       string
	Kind       string // metal (default) or alien
	Commission bool   // alien only: run commission.yml before setup_bot.yml
	OnProgress ProgressFunc
}

const (
	probeConnectTimeout = 5 // seconds, ssh ConnectTimeout
	probeTimeout        = 20 * time.Second
	// Playbooks that rewrite machine identity. Aliens get commission only
	// through AddMachine; metal never.
	commissionPlaybook = "commission.yml"
	setupBotPlaybook   = "setup_bot.yml"
)

// probeScript prints os-release fields and whether passwordless sudo works.
// ssh joins argv with spaces, so this is one remote shell line.
const probeScript = `. /etc/os-release 2>/dev/null; echo "ID=$ID"; echo "ID_LIKE=$ID_LIKE"; echo "PRETTY_NAME=$PRETTY_NAME"; sudo -n true 2>/dev/null && echo SUDO=ok`

type probeResult struct {
	ID     string
	IDLike string
	Pretty string
	Sudo   bool
}

func parseProbe(out string) probeResult {
	var r probeResult
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "ID":
			r.ID = v
		case "ID_LIKE":
			r.IDLike = v
		case "PRETTY_NAME":
			r.Pretty = v
		case "SUDO":
			r.Sudo = v == "ok"
		}
	}
	return r
}

// ubuntuCompatible accepts Ubuntu, Debian, and derivatives that say so in
// ID_LIKE (Mint, Pop!_OS, ...).
func (r probeResult) ubuntuCompatible() bool {
	for _, id := range append([]string{r.ID}, strings.Fields(r.IDLike)...) {
		if id == "ubuntu" || id == "debian" {
			return true
		}
	}
	return false
}

// runRemote runs c, capturing output. A non-zero remote exit is a result, not
// an error; failing to run ssh or a timeout is a bad gateway. Exit 255 is
// left to the caller: it is ssh's own failure, or a remote command's choice.
func runRemote(op string, c *exec.Cmd, timeout time.Duration) (*CmdResult, error) {
	var stdout, stderr bytes.Buffer
	c.Stdin = nil
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Start(); err != nil {
		return nil, badGateway(op, err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	var err error
	if timeout > 0 {
		select {
		case err = <-done:
		case <-time.After(timeout):
			_ = c.Process.Kill()
			<-done
			return nil, badGateway(op, fmt.Errorf("timed out after %s", timeout))
		}
	} else {
		err = <-done
	}
	res := &CmdResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return res, nil
	}
	if e, ok := err.(*exec.ExitError); ok {
		res.ExitCode = e.ExitCode()
		return res, nil
	}
	return res, badGateway(op, err)
}

// sshFailed turns exit 255 from a command vmtool chose (which never exits
// 255 itself) into a bad gateway.
func sshFailed(op string, res *CmdResult) error {
	if res.ExitCode != 255 {
		return nil
	}
	detail := strings.TrimSpace(res.Stderr)
	if detail == "" {
		detail = "exit status 255"
	}
	return badGateway(op, fmt.Errorf("ssh failed: %s", detail))
}

func probeMachine(m vmtool.Machine) (probeResult, error) {
	res, err := runRemote("probe", vmtool.MachineRemoteCmd(m, probeConnectTimeout, []string{probeScript}), probeTimeout)
	if err == nil {
		err = sshFailed("probe", res)
	}
	if err != nil {
		return probeResult{}, err
	}
	return parseProbe(res.Stdout), nil
}

func machineInfo(m vmtool.Machine) MachineInfo {
	info := MachineInfo{Machine: m, State: MachineReachable}
	r, err := probeMachine(m)
	if err != nil {
		info.State = MachineUnreachable
		info.Error = err.Error()
		return info
	}
	info.OS = r.Pretty
	return info
}

func loadMachine(op, name string) (*vmtool.Machine, error) {
	m, err := vmtool.LoadMachine(name)
	if err != nil {
		return nil, wrap(op, err)
	}
	return m, nil
}

// ListMachines returns registered machines, probed concurrently.
func (s *Service) ListMachines(_ context.Context) ([]MachineInfo, error) {
	ms, err := vmtool.ListMachines()
	if err != nil {
		return nil, wrap("list_machines", err)
	}
	out := make([]MachineInfo, len(ms))
	var wg sync.WaitGroup
	for i := range ms {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = machineInfo(ms[i])
		}(i)
	}
	wg.Wait()
	return out, nil
}

func (s *Service) GetMachine(_ context.Context, name string) (*MachineInfo, error) {
	m, err := loadMachine("get_machine", name)
	if err != nil {
		return nil, err
	}
	info := machineInfo(*m)
	return &info, nil
}

// RemoveMachine forgets the machine locally. The machine is not touched.
func (s *Service) RemoveMachine(_ context.Context, name string) error {
	return wrap("remove_machine", vmtool.RemoveMachine(name))
}

// AddMachine registers and onboards a metal/alien machine. vms is used to
// refuse names that are libvirt domains; nil skips that check.
func (s *Service) AddMachine(_ context.Context, vms Manager, in AddMachineInput) (*MachineInfo, error) {
	const op = "add_machine"
	kind := vmtool.MachineKind(in.Kind)
	if kind == "" {
		kind = vmtool.MachineMetal
	}
	switch {
	case !vmtool.ValidMachineName(in.Name):
		return nil, invalid(op, fmt.Errorf("invalid name %q: use letters, digits, '.', '_', '-'", in.Name))
	case in.Host == "":
		return nil, invalid(op, fmt.Errorf("host is required"))
	case !vmtool.ValidMachineKind(kind):
		return nil, invalid(op, fmt.Errorf("invalid kind %q: want metal or alien", in.Kind))
	case in.Commission && kind != vmtool.MachineAlien:
		return nil, invalid(op, fmt.Errorf("commission is only for alien machines"))
	case slices.Contains(s.HostNames, in.Name):
		return nil, conflict(op, fmt.Errorf("%q is a vmtool host name; .machines/%s/ holds that host's VM credentials", in.Name, in.Name))
	}

	if vms != nil {
		_, err := vms.Info(in.Name)
		if err == nil {
			return nil, conflict(op, fmt.Errorf("%q is a libvirt VM", in.Name))
		}
		if !vmtool.IsNotFound(err) {
			return nil, wrap(op, err)
		}
	}
	m := vmtool.Machine{Name: in.Name, Kind: kind, Host: in.Host}
	if existing, err := vmtool.LoadMachine(in.Name); err == nil {
		if existing.Host != m.Host || existing.Kind != m.Kind {
			return nil, conflict(op, fmt.Errorf("%q is already registered as %s at %s", in.Name, existing.Kind, existing.Host))
		}
	} else if !vmtool.IsNotFound(err) {
		return nil, wrap(op, err)
	} else if vmtool.HasCredentials(in.Name) {
		return nil, conflict(op, fmt.Errorf(".machines/%s holds credentials from something else (a VM?); remove it or pick another name", in.Name))
	}

	progress := in.OnProgress
	progress.emit("probe", "start", "checking ssh as "+m.Auth().User+"@"+m.Host, "")
	r, err := probeMachine(m)
	if err != nil {
		progress.emit("probe", "error", err.Error(), "")
		return nil, badGateway(op, fmt.Errorf("%v (does it accept packer/packer? see ansible/playbooks/prep_machine.yml)", err))
	}
	if !r.ubuntuCompatible() {
		err := conflict(op, fmt.Errorf("%s runs %q; vmtool needs an Ubuntu-compatible OS", m.Host, r.Pretty))
		progress.emit("probe", "error", err.Error(), "")
		return nil, err
	}
	if !r.Sudo {
		err := conflict(op, fmt.Errorf("%s@%s has no passwordless sudo", m.Auth().User, m.Host))
		progress.emit("probe", "error", err.Error(), "")
		return nil, err
	}
	progress.emit("probe", "done", r.Pretty, "")

	if err := vmtool.SaveMachine(m); err != nil {
		return nil, wrap(op, err)
	}

	if in.Commission {
		if err := s.machinePlaybook(op, m, commissionPlaybook, nil, progress); err != nil {
			return nil, err
		}
	}
	if err := s.machinePlaybook(op, m, setupBotPlaybook, nil, progress); err != nil {
		return nil, err
	}

	// setup_bot.yml rewrote .machines/<name>/ creds; prove they work.
	info := machineInfo(m)
	if info.State != MachineReachable {
		return &info, badGateway(op, fmt.Errorf("setup_bot.yml finished but ssh as %s failed: %s", m.Auth().User, info.Error))
	}
	return &info, nil
}

func (s *Service) machinePlaybook(op string, m vmtool.Machine, playbook string, extra map[string]string, progress ProgressFunc) error {
	pb, err := s.resolvePlaybook(playbook)
	if err != nil {
		return err
	}
	progress.emit("playbook", "start", "running "+playbook, "")
	auth := m.Auth()
	dir, err := vmtool.AbsMachineDir(m.Name)
	if err != nil {
		progress.emit("playbook", "error", err.Error(), "")
		return wrap(op, err)
	}
	invPath, cleanup, err := vmtool.TempInventory(m.Name, m.Host, auth, dir)
	if err != nil {
		progress.emit("playbook", "error", err.Error(), "")
		return wrap(op, err)
	}
	defer cleanup()
	out, err := vmtool.RunPlaybook(vmtool.Tunnel{}, invPath, pb, withMachineVars(m, extra))
	if err != nil {
		progress.emit("playbook", "error", err.Error(), out)
		return &Error{Kind: KindInternal, Op: op, Err: err, Output: out}
	}
	progress.emit("playbook", "done", playbook+" done", out)
	return nil
}

// withMachineVars sets deviceid and vmtool_kind unless extra already has them.
func withMachineVars(m vmtool.Machine, extra map[string]string) map[string]string {
	out := withDeviceID(m.Name, extra)
	if _, ok := out["vmtool_kind"]; !ok {
		out["vmtool_kind"] = string(m.Kind)
	}
	return out
}

func (s *Service) RunMachinePlaybook(_ context.Context, name, playbook string, extraVars map[string]string) (*PlaybookResult, error) {
	const op = "machine_playbook"
	if filepath.Base(playbook) == commissionPlaybook {
		return nil, invalid(op, fmt.Errorf("%s rewrites machine identity; for aliens use add with commission", commissionPlaybook))
	}
	if _, err := s.resolvePlaybook(playbook); err != nil {
		return nil, err
	}
	m, err := loadMachine(op, name)
	if err != nil {
		return nil, err
	}
	var res PlaybookResult
	err = s.machinePlaybook(op, *m, playbook, extraVars, func(ev ProgressEvent) {
		if ev.Output != "" {
			res.Output = ev.Output
		}
	})
	return &res, err
}

func (s *Service) RunMachineCommand(_ context.Context, name string, argv []string) (*CmdResult, error) {
	const op = "machine_cmd"
	if len(argv) == 0 {
		return nil, invalid(op, fmt.Errorf("command is required"))
	}
	m, err := loadMachine(op, name)
	if err != nil {
		return nil, err
	}
	return runRemote(op, vmtool.MachineRemoteCmd(*m, 0, argv), 0)
}

// RebootMachine schedules a reboot one second out so ssh returns cleanly
// before the connection drops.
func (s *Service) RebootMachine(_ context.Context, name string) (*MachineInfo, error) {
	const op = "machine_reboot"
	m, err := loadMachine(op, name)
	if err != nil {
		return nil, err
	}
	argv := []string{"sudo", "-n", "systemd-run", "--on-active=1", "systemctl", "reboot"}
	res, err := runRemote(op, vmtool.MachineRemoteCmd(*m, probeConnectTimeout, argv), probeTimeout)
	if err == nil {
		err = sshFailed(op, res)
	}
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, internal(op, fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr)))
	}
	return &MachineInfo{Machine: *m, State: MachineRebooting}, nil
}

// MachineSSHCmd builds an interactive ssh command for CLI, TUI, and console.
func (s *Service) MachineSSHCmd(name string, extraArgs ...string) (*exec.Cmd, error) {
	m, err := loadMachine("machine_ssh", name)
	if err != nil {
		return nil, err
	}
	return vmtool.MachineSSHCmd(*m, extraArgs...), nil
}
