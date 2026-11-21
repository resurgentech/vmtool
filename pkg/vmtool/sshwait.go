package vmtool

import (
	"fmt"
	"os/exec"
	"time"
)

// SSHProbeInterval is the pause between two failed SSH probes.
const SSHProbeInterval = time.Second

// sshProbeConnectTimeout is ssh's ConnectTimeout for one probe, in seconds.
const sshProbeConnectTimeout = "2"

// SSHClock is the time source of WaitSSH. The zero value is the wall clock;
// tests set both fields to run a wait on a fake one.
type SSHClock struct {
	Now   func() time.Time
	Sleep func(time.Duration)
}

func (c SSHClock) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c SSHClock) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// ProbeSSH builds one `ssh ... true` to VM name on host through the tunnel,
// with exactly auth (not the credentials fallback vmSSH applies) and the
// same host-key options as the inventory. It exits 0 only when the login and
// the remote true succeed. A password login goes through `sshpass -e` with
// the password in SSHPASS, never in argv.
//
// BatchMode=yes (no prompt) is set for a key login only: OpenSSH's BatchMode
// also turns password authentication off, so a password login instead gets
// NumberOfPasswordPrompts=1, which makes a wrong password fail at once.
func ProbeSSH(t Tunnel, host, name string, auth Auth) (*exec.Cmd, error) {
	if err := validVMKey(host, name); err != nil {
		return nil, err
	}
	if err := t.check(); err != nil {
		return nil, err
	}
	opts := []string{
		"-o", "ProxyCommand=" + t.ProxyCommand(host, name),
		"-o", "ConnectTimeout=" + sshProbeConnectTimeout,
	}
	if auth.KeyPath == "" && auth.Password != "" {
		opts = append(opts, "-o", "NumberOfPasswordPrompts=1")
	} else {
		opts = append(opts, "-o", "BatchMode=yes")
	}
	cmd := buildSSH(host+"/"+name, true, auth, opts, []string{"true"})
	cmd.Env = t.Environ(cmd.Environ())
	return cmd, nil
}

// WaitSSH blocks until a login to VM name on host through the tunnel works
// (ProbeSSH), retrying every SSHProbeInterval for at most timeout. It
// returns the last attempt's combined output, on failure too.
func WaitSSH(t Tunnel, host, name string, auth Auth, timeout time.Duration, clk SSHClock) (string, error) {
	deadline := clk.now().Add(timeout)
	for {
		cmd, err := ProbeSSH(t, host, name, auth)
		if err != nil {
			return "", fmt.Errorf("waiting for SSH: %w", err)
		}
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), nil
		}
		if cmd.Err != nil { // refused to start (a missing tool)
			return string(out), fmt.Errorf("waiting for SSH: %w", err)
		}
		if !clk.now().Add(SSHProbeInterval).Before(deadline) {
			return string(out), fmt.Errorf("waiting for SSH: %w", err)
		}
		clk.sleep(SSHProbeInterval)
	}
}
