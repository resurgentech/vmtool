package vmtool

import (
	"os"
	"os/exec"
	"path/filepath"

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

// ResolveAuth returns .machines credentials for ip or name when present,
// otherwise fallback with packer defaults filled in.
func ResolveAuth(ip, name string, fallback Auth) Auth {
	for _, id := range []string{ip, name} {
		if id == "" {
			continue
		}
		if a, ok := loadMachinesAuth(id); ok {
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

func loadMachinesAuth(id string) (Auth, bool) {
	dir := filepath.Join(".machines", id)
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

// ForgetMachine removes stored .machines credentials for each non-empty id.
// Call it when a VM's disk is replaced or destroyed; stale creds would
// otherwise win over packer defaults in ResolveAuth. Registered metal/alien
// machines (entries with a machine.yml) are never removed here.
func ForgetMachine(ids ...string) error {
	for _, id := range ids {
		if id == "" || id != filepath.Base(id) || id == "." || id == ".." {
			continue
		}
		if IsMachine(id) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(".machines", id)); err != nil {
			return err
		}
	}
	return nil
}

// SSHCmd builds an interactive ssh command for the guest.
// extraArgs are appended after user@host (remote command or ssh operands).
func SSHCmd(ip, name string, extraArgs ...string) (*exec.Cmd, error) {
	cmd := sshCommand(ip, ResolveAuth(ip, name, DefaultAuth()), extraArgs)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd, nil
}

// RemoteCmd builds a non-interactive ssh command that runs argv on the guest.
func RemoteCmd(ip, name string, argv []string) (*exec.Cmd, error) {
	return sshCommand(ip, ResolveAuth(ip, name, DefaultAuth()), argv), nil
}

func sshCommand(ip string, auth Auth, remote []string) *exec.Cmd {
	return sshCommandOpts(ip, auth, nil, remote)
}

// sshCommandOpts is sshCommand with extra ssh options placed before the
// destination.
func sshCommandOpts(ip string, auth Auth, opts, remote []string) *exec.Cmd {
	args := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	args = append(args, opts...)
	if auth.KeyPath != "" {
		args = append(args, "-o", "IdentitiesOnly=yes", "-i", auth.KeyPath)
	}
	dest := ip
	if auth.User != "" {
		dest = auth.User + "@" + ip
	}
	args = append(args, dest)
	args = append(args, remote...)
	if auth.KeyPath == "" && auth.Password != "" {
		cmd := exec.Command("sshpass", append([]string{"-e", "ssh"}, args...)...)
		cmd.Env = append(os.Environ(), "SSHPASS="+auth.Password)
		return cmd
	}
	return exec.Command("ssh", args...)
}
