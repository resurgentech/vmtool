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
		{"vmtool_packages_debian", "ovmf"},
		{"vmtool_packages_arch", "edk2-ovmf"},
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
