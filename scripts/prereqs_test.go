package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jaredeh/vmtool/internal/testexec"
)

// prereqs.sh <host> hands the playbook the hypervisor role and every layout
// variable the justfile owns.
func TestPrereqsHostHypervisorArgs(t *testing.T) {
	e := newDeployEnv(t, "")
	env := append(e.env, "HV_DIR=/opt/vmtool", "HV_POOL_DIR=/var/lib/vmtool/images",
		"GO_MIN=1.21", "GO_INSTALL_VERSION=1.24.0")
	r := runScript(t, e.stub, e.wd, "prereqs.sh", env, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv := readOr(t, filepath.Join(e.stub, "ansible-playbook.argv"))
	for _, w := range []string{
		"-e vmtool_role=hypervisor", "-e vmtool_hv_dir=/opt/vmtool",
		"-e vmtool_hv_pool_dir=/var/lib/vmtool/images", "-e vmtool_go_min=1.21",
		"-e vmtool_go_install_version=1.24.0", "-e vmtool_install_go=true",
		"-e vmtool_daemon_port=9473", "--ask-become-pass",
	} {
		if !strings.Contains(argv, w) {
			t.Errorf("argv lacks %q: %q", w, argv)
		}
	}
}

// Without the justfile's variables the script falls back to the same values.
func TestPrereqsHostDefaults(t *testing.T) {
	e := newDeployEnv(t, "")
	r := runScript(t, e.stub, e.wd, "prereqs.sh", e.env, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv := readOr(t, filepath.Join(e.stub, "ansible-playbook.argv"))
	for _, w := range []string{"-e vmtool_hv_dir=/opt/vmtool", "-e vmtool_hv_pool_dir=/var/lib/vmtool/images"} {
		if !strings.Contains(argv, w) {
			t.Errorf("argv lacks %q: %q", w, argv)
		}
	}
	if !regexp.MustCompile(`-e vmtool_go_install_version=\d+\.\d+(\.\d+)?( |$)`).MatchString(argv) {
		t.Errorf("no go install version: %q", argv)
	}
}

func TestPrereqsLocalHasNoHypervisorArgs(t *testing.T) {
	e := newDeployEnv(t, "")
	env := append(e.env, "HV_DIR=/opt/vmtool", "GO_INSTALL_VERSION=1.24.0")
	r := runScript(t, e.stub, e.wd, "prereqs.sh", env)
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv := readOr(t, filepath.Join(e.stub, "ansible-playbook.argv"))
	for _, bad := range []string{"vmtool_role", "vmtool_hv_", "vmtool_go_", "vmtool_install_go"} {
		if strings.Contains(argv, bad) {
			t.Errorf("local argv has %q: %q", bad, argv)
		}
	}
}

func justVar(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^` + name + ` := "([^"]*)"`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("justfile lacks variable %s", name)
	}
	return string(m[1])
}

func TestJustfileHypervisorVars(t *testing.T) {
	for k, v := range map[string]string{
		"hv_dir": "/opt/vmtool", "hv_pool_dir": "/var/lib/vmtool/images", "go_min": "1.21",
	} {
		if got := justVar(t, k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	b, _ := os.ReadFile("../justfile")
	if regexp.MustCompile(`(?m)^remote_dir\b`).Match(b) || strings.Contains(string(b), "{{ remote_dir }}") {
		t.Error("remote_dir still in justfile")
	}
	mod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^go (\S+)$`).FindSubmatch(mod)
	if m == nil {
		t.Fatal("go.mod has no go line")
	}
	if got := justVar(t, "go_install_version"); got != string(m[1]) {
		t.Errorf("go_install_version %q != go.mod go line %q", got, m[1])
	}
}

func TestJustPrereqsPassesLayout(t *testing.T) {
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just not installed")
	}
	out, err := exec.Command("just", "--justfile", "../justfile", "--dry-run", "prereqs", "h1").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, w := range []string{`HV_DIR="/opt/vmtool"`, `HV_POOL_DIR="/var/lib/vmtool/images"`, `GO_MIN="1.21"`, "GO_INSTALL_VERSION="} {
		if !strings.Contains(string(out), w) {
			t.Errorf("prereqs recipe lacks %q: %s", w, out)
		}
	}
}

// prereqsArgvStub is a stand-in prereqs.sh: it writes each argument it gets,
// NUL-terminated, to the file named by $VMTOOL_TEST_PREREQS_ARGV.
const prereqsArgvStub = `#!/bin/sh
: > "$VMTOOL_TEST_PREREQS_ARGV"
for a in "$@"; do
	printf '%s\0' "$a" >> "$VMTOOL_TEST_PREREQS_ARGV"
done
`

// justPrereqsArgv runs the real justfile's prereqs recipe with args as
// separate argv elements (what a shell hands just after the user's quoting),
// with {{ root }}/scripts/prereqs.sh being the stub, and returns the
// arguments the stub received. Runs no Ansible and no real prereqs.sh.
func justPrereqsArgv(t *testing.T, args ...string) []string {
	t.Helper()
	stub := testexec.Stub(t, map[string]string{"prereqs.sh": prereqsArgvStub})
	root := t.TempDir()
	jf, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "justfile"), jf, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stub, filepath.Join(root, "scripts")); err != nil {
		t.Fatal(err)
	}
	argvFile := filepath.Join(t.TempDir(), "argv")
	cmd := exec.Command("just", append([]string{"--justfile", filepath.Join(root, "justfile"), "prereqs"}, args...)...)
	cmd.Env = append(os.Environ(), "VMTOOL_TEST_PREREQS_ARGV="+argvFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("just prereqs %q: %v\n%s", args, err, out)
	}
	b, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("stub prereqs.sh did not run: %v", err)
	}
	got := strings.Split(string(b), "\x00")
	return got[:len(got)-1] // the last NUL terminates, it does not separate
}

func sameArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const docNvmeJSON = `{"vmtool_nvme_drives":["/dev/disk/by-id/nvme-A","/dev/disk/by-id/nvme-B"]}`

// L9: `just prereqs` hands prereqs.sh each argument unchanged.
func TestJustPrereqsPassesArgsVerbatim(t *testing.T) {
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just not installed")
	}
	for _, args := range [][]string{
		{"veritech02", "-e", docNvmeJSON},
		{"PLACEHOLDER-HOST", "-e", "b c $HOME"},
		{},
	} {
		if got := justPrereqsArgv(t, args...); !sameArgs(got, args) {
			t.Errorf("just prereqs %q: prereqs.sh got %d args %q", args, len(got), got)
		}
	}
}

// L9: the NVMe command is documented the same, byte for byte, in the README,
// the header of prereqs.sh and the header of nvme_stripe.yml, and works as
// written. T1: the thin-pool command (the one that sets vmtool_nvme_thin) is
// documented the same in prereqs.sh and nvme_stripe.yml, checked separately.
func TestPrereqsDocsAgree(t *testing.T) {
	re := regexp.MustCompile(`just prereqs (\S+) -e '([^']*)'`)
	check := func(thin bool, files []string) {
		var lines []string
		var m []string
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var hits []string
			for _, l := range strings.Split(string(b), "\n") {
				l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#"))
				if mm := re.FindStringSubmatch(l); mm != nil && strings.Contains(mm[2], "vmtool_nvme_drives") && strings.Contains(mm[2], "vmtool_nvme_thin") == thin {
					hits = append(hits, l)
					m = mm
				}
			}
			if len(hits) != 1 {
				t.Errorf("%s: want exactly one `just prereqs <host> -e '<json>'` line (thin=%v), got %q", f, thin, hits)
				continue
			}
			lines = append(lines, hits[0])
		}
		if t.Failed() {
			return
		}
		for _, l := range lines[1:] {
			if l != lines[0] {
				t.Fatalf("the documented commands differ:\n%s", strings.Join(lines, "\n"))
			}
		}
		var v struct {
			Drives []string `json:"vmtool_nvme_drives"`
			Thin   bool     `json:"vmtool_nvme_thin"`
		}
		if err := json.Unmarshal([]byte(m[2]), &v); err != nil {
			t.Fatalf("documented JSON %q: %v", m[2], err)
		}
		if v.Thin != thin {
			t.Errorf("documented vmtool_nvme_thin = %v, want %v", v.Thin, thin)
		}
		if len(v.Drives) < 2 {
			t.Errorf("documented vmtool_nvme_drives has %d drives, want at least 2", len(v.Drives))
		}
		for _, d := range v.Drives {
			if !strings.HasPrefix(d, "/dev/disk/by-id/") {
				t.Errorf("documented drive %q is not a /dev/disk/by-id/ path", d)
			}
		}
		if _, err := exec.LookPath("just"); err != nil {
			return
		}
		want := []string{m[1], "-e", m[2]}
		if got := justPrereqsArgv(t, want...); !sameArgs(got, want) {
			t.Errorf("the documented command reaches prereqs.sh as %q, want %q", got, want)
		}
	}
	check(false, []string{"../README.md", "prereqs.sh", nvmeStripePath})
	check(true, []string{"../README.md", "prereqs.sh", nvmeStripePath})
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just not installed; the commands were not run")
	}
}

func ansibleEnv(t *testing.T) []string {
	return append(os.Environ(), "ANSIBLE_LOCAL_TEMP="+t.TempDir(), "ANSIBLE_HOME="+t.TempDir())
}

// The Go-version decision is the one Jinja expression vmtool_go_ok in
// setup_host.yml; evaluate that very string with ansible's own `version` test.
// `ansible localhost -m debug` over the local connection, no become: read-only.
func TestGoVersionDecision(t *testing.T) {
	if _, err := exec.LookPath("ansible"); err != nil {
		t.Skip("ansible not installed")
	}
	b, err := os.ReadFile("../ansible/playbooks/setup_host.yml")
	if err != nil {
		t.Fatal(err)
	}
	var plays []struct {
		Vars map[string]any `yaml:"vars"`
	}
	if err := yaml.Unmarshal(b, &plays); err != nil {
		t.Fatal(err)
	}
	expr, _ := plays[0].Vars["vmtool_go_ok"].(string)
	if expr == "" {
		t.Fatal("setup_host.yml has no vars.vmtool_go_ok")
	}
	for _, c := range []struct {
		raw  string
		want bool
	}{
		{"go1.18.1", false}, {"go1.20.14", false}, {"go1.21.0", true}, {"go1.22.2", true},
		{"go1.24rc1", true}, {"go1.24.0\n", true}, {"", false}, {"go", false}, {"garbage", false},
	} {
		vars, _ := json.Marshal(map[string]string{"vmtool_go_ok": expr, "vmtool_go_raw": c.raw, "vmtool_go_min": "1.21"})
		vf := filepath.Join(t.TempDir(), "v.json")
		os.WriteFile(vf, vars, 0o600)
		cmd := exec.Command("ansible", "localhost", "-i", "localhost,", "-c", "local",
			"-m", "ansible.builtin.debug", "-a", "var=vmtool_go_ok", "-e", "@"+vf)
		cmd.Env = ansibleEnv(t)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%q: %v\n%s", c.raw, err, out)
		}
		want := `"vmtool_go_ok": false`
		if c.want {
			want = `"vmtool_go_ok": true`
		}
		if !strings.Contains(string(out), want) {
			t.Errorf("raw %q: want %s, got %s", c.raw, want, out)
		}
	}
}

func TestSetupHostHypervisorSyntax(t *testing.T) {
	b, err := os.ReadFile("../ansible/playbooks/setup_host.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"vmtool_role", "vmtool_hv_dir", "vmtool_hv_pool_dir", "vmtool_go_install_version", "dl.google.com/go/"} {
		if !strings.Contains(string(b), w) {
			t.Errorf("setup_host.yml lacks %q", w)
		}
	}
	if strings.Contains(string(b), "golang-go") {
		t.Error("golang-go still in the package list")
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook not installed")
	}
	cmd := exec.Command("ansible-playbook", "--syntax-check", "-i", "localhost,",
		"-e", "vmtool_role=hypervisor", "-e", "vmtool_install_go=true", "../ansible/playbooks/setup_host.yml")
	cmd.Env = ansibleEnv(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("syntax-check: %v\n%s", err, out)
	}
}

// TestUserTasksAppendGroups guards against the user module replacing an
// account's supplementary groups (which would drop the login user from sudo):
// every user task that sets groups must set append: true as a module option,
// not under vars.
func TestUserTasksAppendGroups(t *testing.T) {
	b, err := os.ReadFile("../ansible/playbooks/setup_host.yml")
	if err != nil {
		t.Fatal(err)
	}
	var plays []struct {
		Tasks []map[string]any `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(b, &plays); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range plays {
		for _, task := range p.Tasks {
			for _, key := range []string{"ansible.builtin.user", "user"} {
				mod, ok := task[key].(map[string]any)
				if !ok || mod["groups"] == nil {
					continue
				}
				n++
				if mod["append"] != true {
					t.Errorf("task %q sets groups without append: true (it would replace the account's groups)", task["name"])
				}
			}
		}
	}
	if n < 2 {
		t.Fatalf("found %d user tasks with groups, want at least 2", n)
	}
}

// TestSetupHostInstallsOVMF pins the UEFI firmware package: new VMs default to
// UEFI and packer's UEFI builds need it on the workstation.
func TestSetupHostInstallsOVMF(t *testing.T) {
	b, err := os.ReadFile("../ansible/playbooks/setup_host.yml")
	if err != nil {
		t.Fatal(err)
	}
	var plays []struct {
		Vars map[string]any `yaml:"vars"`
	}
	if err := yaml.Unmarshal(b, &plays); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ list, pkg string }{
		{"vmtool_packages_vm_debian", "ovmf"},
		{"vmtool_packages_vm_arch", "edk2-ovmf"},
	} {
		found := false
		for _, p := range plays {
			items, _ := p.Vars[c.list].([]any)
			for _, it := range items {
				if it == c.pkg {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("%s lacks %q", c.list, c.pkg)
		}
	}
}

// A listed host's own port goes to the playbook (the ufw rule).
func TestPrereqsHostPort(t *testing.T) {
	e := newDeployEnv(t, "")
	addStub(t, e.stub, "vmtool", `if [[ "$1 $2" == "config host" ]]; then echo "10.0.0.5 jared 9474"; fi`)
	r := runScript(t, e.stub, e.wd, "prereqs.sh", e.env, "PLACEHOLDER-HOST")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv := readOr(t, filepath.Join(e.stub, "ansible-playbook.argv"))
	if !strings.Contains(argv, "-e vmtool_daemon_port=9474") || strings.Contains(argv, "9473") {
		t.Errorf("argv %q", argv)
	}
	if inv := readOr(t, filepath.Join(e.stub, "inventory.copy")); !strings.Contains(inv, "PLACEHOLDER-HOST ansible_host=10.0.0.5 ansible_user=jared") || strings.Contains(inv, "ansible_connection") {
		t.Errorf("inventory %q", inv)
	}
}

// This machine (the `hostname` stub; config.yml has no name:) is run with
// ansible_connection=local and no ssh, listed or not.
func TestPrereqsThisMachine(t *testing.T) {
	cases := []struct {
		name, listed string
		env          []string
		port         string
	}{
		{"unlisted default", "", nil, "9474"},
		{"unlisted self port", "", []string{"VMTOOL_SELF_PORT=9480"}, "9480"},
		{"listed", "10.0.0.9 ops 9475", nil, "9475"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDeployEnv(t, "")
			os.WriteFile(filepath.Join(e.wd, "config.yml"), []byte("role: workstation\n"), 0o644)
			addStub(t, e.stub, "hostname", "echo PLACEHOLDER-HOST")
			addStub(t, e.stub, "ssh", "exit 99")
			addStub(t, e.stub, "sudo", "exit 99")
			body := `if [[ "$1 $2" == "config host" ]]; then echo 'Error: unknown host "PLACEHOLDER-HOST" in config.yml' >&2; exit 1; fi`
			if c.listed != "" {
				body = `if [[ "$1 $2" == "config host" ]]; then echo "` + c.listed + `"; fi`
			}
			addStub(t, e.stub, "vmtool", body)
			r := runScript(t, e.stub, e.wd, "prereqs.sh", append(e.env, c.env...), "PLACEHOLDER-HOST")
			if r.code != 0 {
				t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
			}
			argv := readOr(t, filepath.Join(e.stub, "ansible-playbook.argv"))
			if !strings.Contains(argv, "-e vmtool_daemon_port="+c.port) || !strings.Contains(argv, "-e vmtool_role=hypervisor") {
				t.Errorf("argv %q", argv)
			}
			if inv := readOr(t, filepath.Join(e.stub, "inventory.copy")); inv != "PLACEHOLDER-HOST ansible_connection=local\n" {
				t.Errorf("inventory %q", inv)
			}
			for _, f := range []string{"ssh.argv", "sudo.argv"} {
				if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
					t.Errorf("%s called", f)
				}
			}
		})
	}
}

// An unlisted name that is not this machine, or any other config host
// failure, is still an error.
func TestPrereqsUnlistedRemoteFails(t *testing.T) {
	e := newDeployEnv(t, "")
	addStub(t, e.stub, "vmtool", `echo 'Error: unknown host "hv1" in config.yml' >&2; exit 1`)
	r := runScript(t, e.stub, e.wd, "prereqs.sh", e.env, "hv1")
	if r.code == 0 || !strings.Contains(r.stderr, "unknown host") {
		t.Fatalf("exit %d: %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.stub, "ansible-playbook.argv")); err == nil {
		t.Error("ansible-playbook ran")
	}
}
