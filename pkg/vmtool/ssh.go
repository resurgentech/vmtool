package vmtool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Auth is SSH credentials for a guest.
type Auth struct {
	User     string
	Password string
	KeyPath  string
}

// DefaultAuth is the image-default packer/packer pair used when no
// .machines entry and no caller credentials are present.
func DefaultAuth() Auth {
	return Auth{User: "packer", Password: "packer"}
}

// SSHNameSuffix ends every VM SSH name: <vm>.<host>.vmtool.
const SSHNameSuffix = ".vmtool"

// SSHName is the name ssh, scp, rsync and sftp use for VM name on host:
// <vm>.<host>.vmtool. It is also the VM's credentials folder name.
func SSHName(host, name string) string {
	return name + "." + host + SSHNameSuffix
}

// ParseSSHName splits <vm>.<host>.vmtool. The suffix must be the lowercase
// ".vmtool"; the rest is split at its last dot, so the VM name may hold dots
// and the host name may not.
func ParseSSHName(s string) (host, vm string, err error) {
	rest, ok := strings.CutSuffix(s, SSHNameSuffix)
	if !ok {
		return "", "", fmt.Errorf("%q: a VM SSH name ends in %q: <vm>.<host>.vmtool", s, SSHNameSuffix)
	}
	i := strings.LastIndexByte(rest, '.')
	if i < 0 {
		return "", "", fmt.Errorf("%q: the VM part is missing: want <vm>.<host>.vmtool", s)
	}
	vm, host = rest[:i], rest[i+1:]
	switch {
	case vm == "":
		return "", "", fmt.Errorf("%q: the VM name is empty: want <vm>.<host>.vmtool", s)
	case host == "":
		return "", "", fmt.Errorf("%q: the host name is empty: want <vm>.<host>.vmtool", s)
	case !ValidHostName(host):
		return "", "", fmt.Errorf("%q: invalid host name %q: letters, digits, '_', '-'", s, host)
	case !ValidMachineName(vm):
		return "", "", fmt.Errorf("%q: invalid VM name %q: letters, digits, '.', '_', '-'", s, vm)
	}
	return host, vm, nil
}

// CredsDir is where a VM's guest credentials live:
// .machines/<vm>.<host>.vmtool, relative to vmtool's working directory.
// setup_bot.yml writes them there (vmtool passes the absolute directory as
// vmtool_machines_dir). It is the only place this path is built.
func CredsDir(host, name string) string {
	return filepath.Join(machinesDir, SSHName(host, name))
}

// AbsCredsDir is CredsDir made absolute, for vmtool_machines_dir.
func AbsCredsDir(host, name string) (string, error) {
	if err := validVMKey(host, name); err != nil {
		return "", err
	}
	return filepath.Abs(CredsDir(host, name))
}

func validVMKey(host, name string) error {
	if !ValidHostName(host) || !ValidMachineName(name) {
		return fmt.Errorf("invalid VM %q/%q: host must be a valid host name (no '.') and name a valid machine name", host, name)
	}
	return nil
}

// ResolveVMAuth returns the credentials stored for VM name on host, or
// fallback with packer defaults filled in. Credentials are keyed by
// (host, vm) only: IPs repeat across hosts, so there is no IP lookup.
func ResolveVMAuth(host, name string, fallback Auth) Auth {
	if validVMKey(host, name) == nil {
		if a, ok := loadAuthDir(CredsDir(host, name)); ok {
			return a
		}
	}
	return defaultedAuth(fallback)
}

func defaultedAuth(a Auth) Auth {
	if a.User == "" {
		a.User = "packer"
	}
	if a.Password == "" && a.KeyPath == "" {
		a.Password = "packer"
	}
	return a
}

// loadAuthDir reads password.yml and id_ed25519 from dir.
func loadAuthDir(dir string) (Auth, bool) {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return Auth{}, false
	}
	var a Auth
	key := filepath.Join(dir, "id_ed25519")
	if _, err := os.Stat(key); err == nil {
		a.KeyPath = key
	}
	if b, err := os.ReadFile(filepath.Join(dir, "password.yml")); err == nil {
		var c struct {
			Username string `yaml:"username"`
			Password string `yaml:"password"`
		}
		if yaml.Unmarshal(b, &c) == nil {
			a.User = c.Username
			a.Password = c.Password
		}
	}
	if a.User == "" && a.KeyPath == "" && a.Password == "" {
		return Auth{}, false
	}
	if a.User == "" {
		a.User = "packer"
	}
	return a, true
}

// ForgetVM removes the stored credentials of VM name on host
// (.machines/<name>.<host>.vmtool/). Call it when the VM's disk is replaced or
// destroyed; stale creds would otherwise win over packer defaults. A
// directory holding a machine.yml is never removed here.
func ForgetVM(host, name string) error {
	if err := validVMKey(host, name); err != nil {
		return err
	}
	dir := CredsDir(host, name)
	if _, err := os.Stat(filepath.Join(dir, machineManifest)); err == nil {
		return nil
	}
	return os.RemoveAll(dir)
}

// SSHCmd builds an interactive ssh command for VM name on host. ssh reaches
// the guest through `vmtool tunnel host/vm` (t), not by IP; the destination is
// host/vm and the user comes from -l. extraArgs follow the destination.
func SSHCmd(t Tunnel, host, name string, extraArgs ...string) (*exec.Cmd, error) {
	cmd, err := vmSSH(t, host, name, extraArgs)
	if err != nil {
		return nil, err
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd, nil
}

// ShellCmd builds an ssh command for the login shell of VM name on host,
// through the tunnel, with no remote command. Unlike SSHCmd it leaves stdio
// to the caller (the server's console gives it a local pty), and opts tune
// ssh: WithTTY puts -tt before the destination.
func ShellCmd(t Tunnel, host, name string, opts ...SSHOption) (*exec.Cmd, error) {
	return vmSSH(t, host, name, nil, opts...)
}

// RemoteCmd builds a non-interactive ssh command that runs argv exactly on VM
// name on host, through the tunnel. Every argument is single-quoted (see
// quoteArgv), because ssh joins its trailing arguments with spaces and the
// guest's shell splits that line.
//
// WithTTY asks for a live terminal (ssh -tt); see SSHOption.
func RemoteCmd(t Tunnel, host, name string, argv []string, opts ...SSHOption) (*exec.Cmd, error) {
	return vmSSH(t, host, name, quoteArgv(argv), opts...)
}

// SSHOption tunes how buildSSH assembles the ssh command. No options is the
// plain command, so existing callers are unchanged.
type SSHOption func(*sshOptions)

type sshOptions struct {
	tty bool
}

// WithTTY puts -tt before the destination: ssh forces a remote pty even when
// its own stdin is not a terminal, puts the local terminal in raw mode when it
// is one, and forwards the window size and every resize.
func WithTTY() SSHOption { return func(o *sshOptions) { o.tty = true } }

func applySSHOptions(opts []SSHOption) sshOptions {
	var o sshOptions
	for _, f := range opts {
		f(&o)
	}
	return o
}

// quoteArg POSIX-single-quotes s: the shell reads it back as that one word.
// A single quote inside s becomes these four characters (close, escaped
// quote, reopen):
//
//	'\''
func quoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// quoteArgv quotes every argument, with no special case for a single one.
func quoteArgv(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = quoteArg(a)
	}
	return out
}

func vmSSH(t Tunnel, host, name string, remote []string, sopts ...SSHOption) (*exec.Cmd, error) {
	if err := validVMKey(host, name); err != nil {
		return nil, err
	}
	if err := t.check(); err != nil {
		return nil, err
	}
	auth := ResolveVMAuth(host, name, DefaultAuth())
	opts := []string{"-o", "ProxyCommand=" + t.ProxyCommand(host, name)}
	cmd := buildSSH(host+"/"+name, true, auth, opts, remote, sopts...)
	cmd.Env = t.Environ(cmd.Environ())
	return cmd, nil
}

func sshCommand(ip string, auth Auth, remote []string) *exec.Cmd {
	return sshCommandOpts(ip, auth, nil, remote)
}

// sshCommandOpts is sshCommand with extra ssh options placed before the
// destination. It is the direct path (metal and alien machines): user@addr,
// no tunnel.
func sshCommandOpts(ip string, auth Auth, opts, remote []string, sopts ...SSHOption) *exec.Cmd {
	return buildSSH(ip, false, auth, opts, remote, sopts...)
}

// buildSSH assembles the ssh (or sshpass -e ssh) command. With loginFlag the
// user is passed as -l and dest stays bare; otherwise it is user@dest.
func buildSSH(dest string, loginFlag bool, auth Auth, opts, remote []string, sopts ...SSHOption) *exec.Cmd {
	o := applySSHOptions(sopts)
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	args = append(args, opts...)
	if auth.KeyPath != "" {
		args = append(args, "-o", "IdentitiesOnly=yes", "-i", auth.KeyPath)
	}
	if auth.User != "" {
		if loginFlag {
			args = append(args, "-l", auth.User)
		} else {
			dest = auth.User + "@" + dest
		}
	}
	if o.tty {
		args = append(args, "-tt")
	}
	args = append(args, dest)
	args = append(args, remote...)
	if auth.KeyPath == "" && auth.Password != "" {
		// A refusal under go test is in cmd.Err: Start and Run return it.
		cmd, _ := command("sshpass", append([]string{"-e", "ssh"}, args...)...)
		cmd.Env = append(childEnviron(), "SSHPASS="+auth.Password)
		return cmd
	}
	cmd, _ := command("ssh", args...)
	cmd.Env = childEnviron()
	return cmd
}
