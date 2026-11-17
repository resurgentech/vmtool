package vmtool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func extraVarArgs(extraVars map[string]string) []string {
	if len(extraVars) == 0 {
		return nil
	}
	keys := make([]string, 0, len(extraVars))
	for k := range extraVars {
		if k == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+extraVars[k])
	}
	return out
}

// ListPlaybooks returns the names of .yml files in the given directory.
func ListPlaybooks(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading playbook directory: %w", err)
	}
	var playbooks []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml") {
			playbooks = append(playbooks, e.Name())
		}
	}
	return playbooks, nil
}

// RunPlaybook executes an ansible-playbook against the given inventory file.
// extraVars are passed as repeated --extra-vars key=value.
// Returns the combined stdout/stderr output and any error.
// t puts the token in the child's environment for the tunnel's ProxyCommand;
// pass Tunnel{} for a metal machine.
func RunPlaybook(t Tunnel, inventoryPath, playbookPath string, extraVars map[string]string) (string, error) {
	cmd := playbookCmd(t, inventoryPath, playbookPath, extraVars)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("running playbook %s: %w", filepath.Base(playbookPath), err)
	}
	return string(out), nil
}

func playbookCmd(t Tunnel, inventoryPath, playbookPath string, extraVars map[string]string) *exec.Cmd {
	args := []string{"--inventory", inventoryPath, playbookPath}
	for _, kv := range extraVarArgs(extraVars) {
		args = append(args, "--extra-vars", kv)
	}
	// A refusal under go test is in cmd.Err; CombinedOutput returns it.
	cmd, _ := command("ansible-playbook", args...)
	cmd.Env = ansibleEnv(t)
	return cmd
}

// RunCommand runs an ad-hoc ansible command on all hosts in the inventory.
// Returns the combined output and any error.
func RunCommand(t Tunnel, inventoryPath, command string) (string, error) {
	cmd := adhocCmd(t, inventoryPath, command)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("running command: %w", err)
	}
	return string(out), nil
}

func adhocCmd(t Tunnel, inventoryPath, shellCmd string) *exec.Cmd {
	cmd, _ := command("ansible", "all",
		"--inventory", inventoryPath,
		"--become",
		"--module-name", "shell",
		"--args", shellCmd,
	)
	cmd.Env = ansibleEnv(t)
	return cmd
}

func ansibleEnv(t Tunnel) []string {
	return t.Environ(append(os.Environ(),
		"ANSIBLE_HOST_KEY_CHECKING=False",
		"ANSIBLE_SSH_ARGS=-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR",
	))
}

// growDiskCommand runs in the guest under /bin/sh (ansible's shell module,
// with --become, so as root). It finds the root partition instead of assuming
// one: the source from `findmnt -n -o SOURCE /` (a btrfs "[/subvol]" suffix
// stripped), its number from <sysblock>/<part>/partition, its disk from
// `lsblk -no PKNAME`. If any of that cannot be resolved (LVM, a whole-disk
// filesystem) it fails with a message rather than guessing.
//
// growpart's output is always printed. Exit 1 with NOCHANGE in the output
// means the partition is already full size (cloud-init got there first) and is
// success; any other nonzero exit fails. The filesystem step then runs either
// way: resize2fs only handles ext2/3/4, btrfs is resized online through its
// mount point, and both are no-ops on a full filesystem.
//
// <sysblock> is ${VMTOOL_SYSBLOCK:-/sys/class/block}; the variable exists so
// tests can point it at a temp dir and is never set in the guest.
const growDiskCommand = `src=$(findmnt -n -o SOURCE /); src=${src%%\[*}; part=${src##*/}; ` +
	`sb=${VMTOOL_SYSBLOCK:-/sys/class/block}; ` +
	`num=$(cat "$sb/$part/partition" 2>/dev/null); ` +
	`disk=$(lsblk -no PKNAME "$src" 2>/dev/null | head -n 1); ` +
	`if [ -z "$part" ] || [ -z "$num" ] || [ -z "$disk" ]; then ` +
	`echo "vmtool grow: cannot find root partition (findmnt source: '$src')" >&2; exit 1; fi; ` +
	`out=$(growpart "/dev/$disk" "$num" 2>&1); rc=$?; printf '%s\n' "$out"; ` +
	`if [ "$rc" -ne 0 ]; then case "$out" in ` +
	`*NOCHANGE*) [ "$rc" -eq 1 ] || exit "$rc" ;; *) exit "$rc" ;; esac; fi; ` +
	`case "$(findmnt -n -o FSTYPE /)" in ` +
	`btrfs) btrfs filesystem resize max / ;; ` +
	`*) resize2fs "$src" ;; esac`

// GrowDisk expands the root partition and filesystem to fill the disk. The
// root partition is found in the guest (see growDiskCommand); ext4 and btrfs
// are handled, and a partition that is already full size is success.
// It returns ansible's combined output, on failure too, so the caller can
// show why. Callers do not retry it blindly.
func GrowDisk(t Tunnel, inventoryPath string) (string, error) {
	out, err := RunCommand(t, inventoryPath, growDiskCommand)
	if err != nil {
		return out, fmt.Errorf("growing disk: %w", err)
	}
	return out, nil
}

// WaitSSH blocks until ansible can reach the guest over SSH, for at most
// timeout (`ansible all -m wait_for_connection -a timeout=<seconds>`). It uses
// the same Tunnel environment and inventory as GrowDisk and returns ansible's
// combined output, on failure too.
func WaitSSH(t Tunnel, inventoryPath string, timeout time.Duration) (string, error) {
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	cmd, _ := command("ansible", "all",
		"--inventory", inventoryPath,
		"--module-name", "wait_for_connection",
		"--args", fmt.Sprintf("timeout=%d", secs),
	)
	cmd.Env = ansibleEnv(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("waiting for SSH: %w", err)
	}
	return string(out), nil
}
