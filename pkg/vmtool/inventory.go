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
	return tempInventoryFile(inventoryYAML(name, ip, "", auth, machinesDir))
}

func tempInventoryFile(content string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "vmtool-inventory-*.yml")
	if err != nil {
		return "", nil, err
	}
	if _, err := f.WriteString(content); err != nil {
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
	return writeInventoryFile(path, inventoryYAML(name, ip, "", auth, machinesDir))
}

func writeInventoryFile(path, content string) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// TempVMInventory is TempInventory for a libvirt VM: the hostname is the VM
// name, ansible_host is host/vm and ansible_ssh_common_args carries the
// ProxyCommand through `vmtool tunnel host/vm`. The token is never written;
// RunPlaybook and GrowDisk put it in the child's environment.
func TempVMInventory(t Tunnel, host, name string, auth Auth, machinesDir string) (path string, cleanup func(), err error) {
	y, err := vmInventoryYAML(t, host, name, auth, machinesDir)
	if err != nil {
		return "", nil, err
	}
	return tempInventoryFile(y)
}

// WriteVMInventory is WriteInventory for a libvirt VM (see TempVMInventory).
func WriteVMInventory(path string, t Tunnel, host, name string, auth Auth, machinesDir string) error {
	y, err := vmInventoryYAML(t, host, name, auth, machinesDir)
	if err != nil {
		return err
	}
	return writeInventoryFile(path, y)
}

// EnsureVMInventory rewrites the inventory unless it already targets
// host/vm. It reports whether it wrote.
func EnsureVMInventory(path string, t Tunnel, host, name string, auth Auth, machinesDir string) (bool, error) {
	data, err := os.ReadFile(path)
	if err == nil && strings.Contains(string(data), "ansible_host: "+host+"/"+name+"\n") {
		return false, nil
	}
	return true, WriteVMInventory(path, t, host, name, auth, machinesDir)
}

func vmInventoryYAML(t Tunnel, host, name string, auth Auth, machinesDir string) (string, error) {
	if err := validVMKey(host, name); err != nil {
		return "", err
	}
	if err := t.check(); err != nil {
		return "", err
	}
	return inventoryYAML(name, host+"/"+name, t.ProxyCommand(host, name), auth, machinesDir), nil
}

// inventoryYAML renders the inventory. addr is ansible_host. proxy, when not
// empty, is the ssh ProxyCommand (VMs); metal and alien machines pass "" and
// are reached directly.
func inventoryYAML(name, addr, proxy string, auth Auth, machinesDir string) string {
	host := name
	if host == "" {
		host = addr
	}
	sshArgs := "-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
	if auth.KeyPath != "" {
		sshArgs += " -o IdentitiesOnly=yes"
	}
	if proxy != "" {
		sshArgs += " -o " + shQuote("ProxyCommand="+proxy)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `all:
  hosts:
    %s:
      ansible_host: %s
      ansible_become: true
      ansible_ssh_user: %s
      ansible_ssh_common_args: %q
`, host, addr, auth.User, sshArgs)
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
