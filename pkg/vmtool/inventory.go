package vmtool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TempInventory writes a fresh inventory to a temp file and returns its path
// and a cleanup function. Caller must call cleanup() when done. machinesDir
// is the absolute credentials directory of the target (AbsCredsDir for a VM,
// AbsMachineDir for metal/alien); it becomes the host var
// vmtool_machines_dir, where setup_bot.yml writes credentials.
func TempInventory(name, ip string, auth Auth, machinesDir string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "vmtool-inventory-*.yml")
	if err != nil {
		return "", nil, err
	}
	if _, err := f.WriteString(inventoryYAML(name, ip, auth, machinesDir)); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// WriteInventory writes or overwrites an Ansible inventory file with the
// given VM's name, IP, SSH credentials and vmtool_machines_dir (see
// TempInventory). If the parent directory does not exist, the write is
// silently skipped.
func WriteInventory(path, name, ip string, auth Auth, machinesDir string) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	return os.WriteFile(path, []byte(inventoryYAML(name, ip, auth, machinesDir)), 0o644)
}

func inventoryYAML(name, ip string, auth Auth, machinesDir string) string {
	host := name
	if host == "" {
		host = ip
	}
	sshArgs := "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
	if auth.KeyPath != "" {
		sshArgs += " -o IdentitiesOnly=yes"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `all:
  hosts:
    %s:
      ansible_host: %s
      ansible_become: true
      ansible_ssh_user: %s
      ansible_ssh_common_args: %q
`, host, ip, auth.User, sshArgs)
	if auth.Password != "" {
		fmt.Fprintf(&b, "      ansible_ssh_pass: %q\n", auth.Password)
	}
	if auth.KeyPath != "" {
		fmt.Fprintf(&b, "      ansible_ssh_private_key_file: %q\n", auth.KeyPath)
	}
	if machinesDir != "" {
		fmt.Fprintf(&b, "      vmtool_machines_dir: %q\n", machinesDir)
	}
	return b.String()
}
