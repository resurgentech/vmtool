package scripts_test

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// V6: setup_host.yml has a switch, vmtool_vm_host (default true). False makes
// the box a network home: libvirt, dnsmasq, the vmtool user and what `just
// deploy` needs to build there, and none of QEMU, OVMF, pools or the NVMe
// stripe. The structure is pinned here; the opt-in syntax check below runs the
// playbook through ansible with the switch off. Nothing here runs ansible
// otherwise.

type setupPlay struct {
	Vars  map[string]any   `yaml:"vars"`
	Tasks []map[string]any `yaml:"tasks"`
}

func readSetupPlay(t *testing.T) setupPlay {
	t.Helper()
	b, err := os.ReadFile(setupHostPath)
	if err != nil {
		t.Fatal(err)
	}
	var plays []setupPlay
	if err := yaml.Unmarshal(b, &plays); err != nil {
		t.Fatal(err)
	}
	if len(plays) != 1 {
		t.Fatalf("%d plays", len(plays))
	}
	return plays[0]
}

func listVar(t *testing.T, p setupPlay, name string) []string {
	t.Helper()
	items, ok := p.Vars[name].([]any)
	if !ok {
		t.Fatalf("%s is not a list: %#v", name, p.Vars[name])
	}
	var out []string
	for _, it := range items {
		s, _ := it.(string)
		out = append(out, s)
	}
	return out
}

func listHas(list []string, pkg string) bool {
	for _, p := range list {
		if p == pkg {
			return true
		}
	}
	return false
}

func TestSetupHostVMHostDefaultsTrue(t *testing.T) {
	p := readSetupPlay(t)
	if p.Vars["vmtool_vm_host"] != true {
		t.Errorf("vmtool_vm_host = %#v, want true (every host is a VM host unless told otherwise)", p.Vars["vmtool_vm_host"])
	}
}

// QEMU and the UEFI firmware are in their own lists, which only a VM host
// installs; the base lists keep what a network home needs.
func TestSetupHostPackagesSplitByVMHost(t *testing.T) {
	p := readSetupPlay(t)
	for _, c := range []struct {
		base, vm string
		vmPkgs   []string
		basePkgs []string
	}{
		{"vmtool_packages_debian", "vmtool_packages_vm_debian",
			[]string{"qemu-kvm", "qemu-utils", "ovmf"},
			[]string{"libvirt-daemon-system", "libvirt-clients", "libvirt-dev", "pkg-config", "dnsmasq-base", "gcc"}},
		{"vmtool_packages_arch", "vmtool_packages_vm_arch",
			[]string{"qemu-desktop", "edk2-ovmf"},
			[]string{"libvirt", "pkgconf", "dnsmasq", "iptables-nft", "gcc"}},
	} {
		base, vm := listVar(t, p, c.base), listVar(t, p, c.vm)
		for _, pkg := range c.vmPkgs {
			if !listHas(vm, pkg) {
				t.Errorf("%s lacks %q", c.vm, pkg)
			}
			if listHas(base, pkg) {
				t.Errorf("%s still has %q: a network home would install it", c.base, pkg)
			}
		}
		for _, pkg := range c.basePkgs {
			if !listHas(base, pkg) {
				t.Errorf("%s lacks %q (a network home needs it)", c.base, pkg)
			}
		}
		for _, pkg := range base {
			if strings.Contains(pkg, "qemu") || strings.Contains(pkg, "ovmf") {
				t.Errorf("%s has %q", c.base, pkg)
			}
		}
	}
	// The list is built from the VM lists only when vmtool_vm_host is true.
	var build map[string]any
	for _, task := range p.Tasks {
		if task["name"] == "Build the package list" {
			build = task
		}
	}
	if build == nil {
		t.Fatal("no 'Build the package list' task")
	}
	vars, _ := build["vars"].(map[string]any)
	var all []string
	for _, v := range vars {
		if s, ok := v.(string); ok {
			all = append(all, s)
		}
	}
	expr := strings.Join(all, "\n")
	for _, w := range []string{"vmtool_vm_host", "vmtool_packages_vm_debian", "vmtool_packages_vm_arch"} {
		if !strings.Contains(expr, w) {
			t.Errorf("the package list expression lacks %q:\n%s", w, expr)
		}
	}
}

// A task that only a VM host needs carries vmtool_vm_host in its when: the
// KVM check, the pools and the NVMe stripe.
func TestSetupHostVMOnlyTasksGuarded(t *testing.T) {
	p := readSetupPlay(t)
	guarded := map[string]bool{}
	for _, task := range p.Tasks {
		name, _ := task["name"].(string)
		switch {
		case name == "Fail when hardware virtualization is unavailable",
			name == "Ensure the storage pools",
			name == "Build the NVMe stripe and its logical pool":
			guarded[name] = true
			if !hasWhen(whenList(task["when"]), "vmtool_vm_host") {
				t.Errorf("task %q lacks a vmtool_vm_host guard (has %v)", name, task["when"])
			}
		}
	}
	for _, n := range []string{"Fail when hardware virtualization is unavailable", "Ensure the storage pools", "Build the NVMe stripe and its logical pool"} {
		if !guarded[n] {
			t.Errorf("no task %q", n)
		}
	}
	// The stripe's own opt-in guard stays.
	for _, task := range p.Tasks {
		if task["ansible.builtin.include_tasks"] == "tasks/nvme_stripe.yml" && !hasWhen(whenList(task["when"]), nvmeGuard) {
			t.Errorf("the stripe include lost its drives guard: %v", task["when"])
		}
	}
}

// Naming pools or drives for a host that is not a VM host is a mistake the
// playbook says so about, before it changes anything; it does not skip them
// silently.
func TestSetupHostRefusesStorageOnNetworkHome(t *testing.T) {
	p := readSetupPlay(t)
	preflight := -1
	firstChange := -1
	for i, task := range p.Tasks {
		name, _ := task["name"].(string)
		if _, ok := task["ansible.builtin.fail"]; ok && strings.Contains(fmt.Sprint(task["when"]), "vmtool_vm_host") &&
			strings.Contains(fmt.Sprint(task["when"]), "vmtool_nvme_drives") {
			preflight = i
			msg := fmt.Sprint(task["ansible.builtin.fail"])
			for _, w := range []string{"vmtool_vm_host", "vmtool_nvme_drives", "vmtool_extra_pools"} {
				if !strings.Contains(msg+fmt.Sprint(task["when"]), w) {
					t.Errorf("the refusal %q does not mention %s", name, w)
				}
			}
		}
		if firstChange < 0 && (task["ansible.builtin.apt"] != nil || task["ansible.builtin.systemd_service"] != nil) {
			firstChange = i
		}
	}
	if preflight < 0 {
		t.Fatal("no fail task for vmtool_nvme_drives / vmtool_extra_pools with vmtool_vm_host false")
	}
	if firstChange >= 0 && preflight > firstChange {
		t.Errorf("the refusal (task %d) comes after the first change (task %d)", preflight, firstChange)
	}
}

// The home still gets the daemon's account, its directories, the firewall
// rules and libvirt: none of those waits for vmtool_vm_host.
func TestSetupHostNetworkHomeKeepsDaemonTasks(t *testing.T) {
	p := readSetupPlay(t)
	for _, name := range []string{
		"Create the vmtool system user", "Create the vmtool directories",
		"Install packages (Debian/Ubuntu)", "Enable and start libvirt units",
		"Define the default network", "Allow VXLAN through ufw from the LAN",
		"Install the upstream Go toolchain",
	} {
		found := false
		for _, task := range p.Tasks {
			if task["name"] != name {
				continue
			}
			found = true
			if strings.Contains(fmt.Sprint(task["when"]), "vmtool_vm_host") {
				t.Errorf("task %q waits for vmtool_vm_host: a network home needs it", name)
			}
		}
		if !found {
			t.Errorf("no task %q", name)
		}
	}
}

// H1: the playbook waits for libvirt with a call a host without QEMU answers.
// `virsh version` needs an emulator ("Cannot find suitable emulator"), so on a
// network home it never succeeds; `virsh list --all` opens a connection to
// the daemon and needs none.
func TestSetupHostLibvirtWaitNeedsNoEmulator(t *testing.T) {
	p := readSetupPlay(t)
	var wait map[string]any
	for _, task := range p.Tasks {
		if task["name"] == "Wait for the system libvirt socket" {
			wait = task
		}
	}
	if wait == nil {
		t.Fatal("no 'Wait for the system libvirt socket' task")
	}
	if got := fmt.Sprint(wait["ansible.builtin.command"]); got != "virsh -c qemu:///system list --all" {
		t.Errorf("the libvirt wait runs %q, want %q", got, "virsh -c qemu:///system list --all")
	}
	if wait["changed_when"] != false || wait["check_mode"] != false {
		t.Errorf("the libvirt wait: changed_when %v, check_mode %v, want false and false", wait["changed_when"], wait["check_mode"])
	}
	if wait["retries"] != 10 || wait["delay"] != 2 {
		t.Errorf("the libvirt wait: retries %v, delay %v, want 10 and 2", wait["retries"], wait["delay"])
	}
	if strings.Contains(fmt.Sprint(wait["when"]), "vmtool_vm_host") {
		t.Errorf("the libvirt wait waits for vmtool_vm_host: a network home needs it")
	}
}

// No playbook and no script asks libvirt for its version.
func TestNoVirshVersionInPlaybooksOrScripts(t *testing.T) {
	re := regexp.MustCompile(`virsh\b[^\n]*\bversion\b`)
	var files []string
	err := filepath.WalkDir("../ansible/playbooks", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sh, err := filepath.Glob("*.sh")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, sh...)
	if len(files) < 3 {
		t.Fatalf("only %d files scanned", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) {
				t.Errorf("%s:%d asks virsh for its version (fails on a host without QEMU): %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// The README says what a home does without QEMU.
func TestReadmeNetworkHomeNeedsNoQEMU(t *testing.T) {
	b, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "#### A network home")
	if i < 0 {
		t.Fatal("README has no network home section")
	}
	sec := s[i:]
	if j := strings.Index(sec[5:], "\n#"); j >= 0 {
		sec = sec[:j+5]
	}
	for _, w := range []string{"A home needs no QEMU", "libvirt manages its networks without it"} {
		if !strings.Contains(sec, w) {
			t.Errorf("README's network home section lacks %q", w)
		}
	}
}

// The README says how to make a network home, and what makes create refuse it.
func TestReadmeDocumentsNetworkHome(t *testing.T) {
	b, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`"vmtool_vm_host":false`, "vms: false", "network home"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("README lacks %q", w)
		}
	}
}

// The syntax check with the switch off, and with it on (the default). Opt-in
// like the other ansible runs: VMTOOL_TEST_ANSIBLE_SYNTAX=1.
func TestSetupHostVMHostSyntax(t *testing.T) {
	if os.Getenv("VMTOOL_TEST_ANSIBLE_SYNTAX") != "1" {
		t.Skip("set VMTOOL_TEST_ANSIBLE_SYNTAX=1 to run ansible-playbook --syntax-check")
	}
	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skip("ansible-playbook not on PATH")
	}
	for _, extra := range [][]string{
		{"-e", `{"vmtool_vm_host":false}`},
		{"-e", `{"vmtool_vm_host":true}`},
		{"-e", "vmtool_role=hypervisor", "-e", `{"vmtool_vm_host":false}`},
	} {
		args := append([]string{"--syntax-check", "-i", "localhost,"}, extra...)
		args = append(args, "../ansible/playbooks/setup_host.yml")
		cmd := exec.Command(bin, args...)
		cmd.Env = ansibleEnv(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("syntax-check %v: %v\n%s", extra, err, out)
		}
	}
}
