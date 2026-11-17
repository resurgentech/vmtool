package vmtool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MachineKind is the kind of a non-libvirt machine in .machines.
type MachineKind string

const (
	// MachineMetal is a physical machine.
	MachineMetal MachineKind = "metal"
	// MachineAlien is a VM vmtool's libvirt cannot see.
	MachineAlien MachineKind = "alien"
)

const (
	machinesDir     = ".machines"
	machineManifest = "machine.yml"

	// MachinesDir is the credentials and registrations directory, relative
	// to vmtool's working directory.
	MachinesDir = machinesDir
	// MachineManifest names a metal/alien registration inside
	// .machines/<name>/. A directory without one is a host directory or a
	// legacy VM credential cache.
	MachineManifest = machineManifest
)

var machineNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Machine is an SSH/Ansible target registered in .machines/<name>/machine.yml.
// Credentials live beside the manifest (password.yml, id_ed25519), the same
// layout setup_bot.yml writes for VMs.
type Machine struct {
	Name string      `yaml:"-"`
	Kind MachineKind `yaml:"kind"`
	Host string      `yaml:"host"`
}

// ValidMachineName reports whether name is safe as a .machines directory and
// Ansible inventory hostname.
func ValidMachineName(name string) bool {
	return machineNameRE.MatchString(name)
}

// ValidHostName reports whether name is a valid vmtool host name: a valid
// machine name without '.', because a VM SSH name <vm>.<host>.vmtool is split
// at its last dot.
func ValidHostName(name string) bool {
	return ValidMachineName(name) && !strings.Contains(name, ".")
}

// ValidMachineKind reports whether k is metal or alien.
func ValidMachineKind(k MachineKind) bool {
	return k == MachineMetal || k == MachineAlien
}

// IsMachine reports whether id has a machine manifest. Such entries are
// registrations, not VM credential caches.
func IsMachine(id string) bool {
	if !ValidMachineName(id) {
		return false
	}
	_, err := os.Stat(filepath.Join(machinesDir, id, machineManifest))
	return err == nil
}

// HasCredentials reports whether .machines/<id>/ itself holds usable
// credentials (a host directory of VM credentials does not).
func HasCredentials(id string) bool {
	if !ValidMachineName(id) {
		return false
	}
	_, ok := loadAuthDir(MachineDir(id))
	return ok
}

// LoadMachine reads .machines/<name>/machine.yml. A missing manifest wraps
// ErrNotFound.
func LoadMachine(name string) (*Machine, error) {
	if !ValidMachineName(name) {
		return nil, fmt.Errorf("machine %q: %w", name, ErrNotFound)
	}
	b, err := os.ReadFile(filepath.Join(machinesDir, name, machineManifest))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("machine %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	m := Machine{Name: name}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("machine %q: parsing %s: %w", name, machineManifest, err)
	}
	if !ValidMachineKind(m.Kind) {
		return nil, fmt.Errorf("machine %q: invalid kind %q", name, m.Kind)
	}
	if m.Host == "" {
		return nil, fmt.Errorf("machine %q: host is required", name)
	}
	return &m, nil
}

// ListMachines returns every registered machine, sorted by name.
func ListMachines() ([]Machine, error) {
	entries, err := os.ReadDir(machinesDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Machine
	for _, e := range entries {
		if !e.IsDir() || !IsMachine(e.Name()) {
			continue
		}
		m, err := LoadMachine(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SaveMachine writes .machines/<name>/machine.yml.
func SaveMachine(m Machine) error {
	if !ValidMachineName(m.Name) {
		return fmt.Errorf("invalid machine name %q", m.Name)
	}
	if !ValidMachineKind(m.Kind) {
		return fmt.Errorf("invalid machine kind %q", m.Kind)
	}
	if m.Host == "" {
		return fmt.Errorf("machine host is required")
	}
	dir := filepath.Join(machinesDir, m.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, machineManifest), b, 0o600)
}

// RemoveMachine deletes .machines/<name>/ (manifest and credentials).
func RemoveMachine(name string) error {
	if _, err := LoadMachine(name); err != nil && IsNotFound(err) {
		return err
	}
	return os.RemoveAll(filepath.Join(machinesDir, name))
}

// Auth is the machine's stored credentials, or packer defaults before
// setup_bot.yml has run. Lookup is by name only: a stale .machines/<ip>
// entry from a VM must not shadow a machine.
func (m Machine) Auth() Auth {
	if ValidMachineName(m.Name) {
		if a, ok := loadAuthDir(MachineDir(m.Name)); ok {
			return a
		}
	}
	return DefaultAuth()
}

// MachineDir is where a metal/alien machine's manifest and credentials
// live: .machines/<name>, relative to vmtool's working directory.
func MachineDir(name string) string {
	return filepath.Join(machinesDir, name)
}

// AbsMachineDir is MachineDir made absolute, for vmtool_machines_dir.
func AbsMachineDir(name string) (string, error) {
	if !ValidMachineName(name) {
		return "", fmt.Errorf("invalid machine name %q", name)
	}
	return filepath.Abs(MachineDir(name))
}

// MachineSSHCmd builds an interactive ssh command for the machine.
func MachineSSHCmd(m Machine, extraArgs ...string) *exec.Cmd {
	cmd := sshCommand(m.Host, m.Auth(), extraArgs)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd
}

// MachineRemoteCmd builds a non-interactive ssh command that runs argv
// exactly on the machine: every argument is single-quoted (see quoteArgv),
// because ssh joins its trailing arguments with spaces. connectTimeout (seconds) bounds the TCP connect; 0 means the
// ssh default.
func MachineRemoteCmd(m Machine, connectTimeout int, argv []string) *exec.Cmd {
	var opts []string
	if connectTimeout > 0 {
		opts = []string{
			"-o", fmt.Sprintf("ConnectTimeout=%d", connectTimeout),
			"-o", "NumberOfPasswordPrompts=1",
		}
	}
	return sshCommandOpts(m.Host, m.Auth(), opts, quoteArgv(argv))
}
