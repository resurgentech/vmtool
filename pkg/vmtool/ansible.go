package vmtool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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
	cmd := exec.Command("ansible-playbook", args...)
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

func adhocCmd(t Tunnel, inventoryPath, command string) *exec.Cmd {
	cmd := exec.Command("ansible", "all",
		"--inventory", inventoryPath,
		"--become",
		"--module-name", "shell",
		"--args", command,
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

// growDiskCommand runs in the guest under /bin/sh. resize2fs only handles
// ext2/3/4; btrfs is resized online through its mount point.
const growDiskCommand = `growpart /dev/vda 2 && case "$(findmnt -n -o FSTYPE /)" in ` +
	`btrfs) btrfs filesystem resize max / ;; ` +
	`*) resize2fs /dev/vda2 ;; esac`

// GrowDisk expands the root partition and filesystem to fill the disk.
// Assumes the packer_images layout: root is /dev/vda2, ext4 or btrfs.
func GrowDisk(t Tunnel, inventoryPath string) error {
	if _, err := RunCommand(t, inventoryPath, growDiskCommand); err != nil {
		return fmt.Errorf("growing disk: %w", err)
	}
	return nil
}
