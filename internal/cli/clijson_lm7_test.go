package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/testexec"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// LM7: --json for the config and metal commands (the net ones are in
// clijson_lm7_net_test.go). The cases join jsonCases; the focused tests pin
// each shape.

const (
	metalPass = "placeholder-metal-password"
	metalKey  = "placeholder-metal-key"
)

func init() {
	jsonCases["vmtool config host"] = jsonCase{setup: configJSONDir("config", "host", "veritech01")}
	jsonCases["vmtool config host-vms"] = jsonCase{setup: configJSONDir("config", "host-vms", "veritech01")}
	jsonCases["vmtool config server-url"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		return storeWorkdir(t, newCLIStore(t), storeTok), []string{"config", "server-url"}
	}}
	jsonCases["vmtool config set-host"] = jsonCase{setup: configJSONDir("config", "set-host", "veritech02", "--address", "192.0.2.9", "--ssh-user", "ubuntu")}
	jsonCases["vmtool config write"] = jsonCase{setup: configJSONDir("config", "write", "--name", "ws")}

	jsonCases["vmtool metal list"] = jsonCase{setup: metalJSONSetup("metal", "list")}
	jsonCases["vmtool metal info"] = jsonCase{setup: metalJSONSetup("metal", "info", "nuc1")}
	jsonCases["vmtool metal rm"] = jsonCase{setup: metalJSONSetup("metal", "rm", "nuc1")}
	jsonCases["vmtool metal reboot"] = jsonCase{setup: metalJSONSetup("metal", "reboot", "nuc1")}
	jsonCases["vmtool metal playbook"] = jsonCase{setup: metalJSONSetup("metal", "playbook", "nuc1", "site.yml")}
	jsonCases["vmtool metal exec"] = jsonCase{setup: metalJSONSetup("metal", "exec", "nuc1", "--", "echo", "hi")}
	jsonCases["vmtool metal add"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		dir, _ := metalJSONSetup()(t)
		return dir, []string{"metal", "add", "nuc2", "192.0.2.8"}
	}}
}

// configJSONDir is a setup for a config command: a working directory whose
// config.yml lists host veritech01, with the host token in the environment.
func configJSONDir(args ...string) func(*testing.T) (string, []string) {
	return func(t *testing.T) (string, []string) {
		t.Setenv(config.TokenEnv, gateToken)
		t.Chdir(t.TempDir())
		return writeWorkdir(t, "name: jetfire\n"+goodConfig(), ""), args
	}
}

// metalStub is the stub ssh of the metal tests: the machine probe answers as
// an Ubuntu box with sudo, a reboot request succeeds, and any other command
// prints "out" and "err" and exits with $CODE (default 0). The password and
// the key of a machine never reach it in a way a test prints.
const metalStub = `#!/bin/sh
case "$*" in
*os-release*) echo ID=ubuntu; echo ID_LIKE=debian; echo "PRETTY_NAME=Ubuntu 24.04 LTS"; echo SUDO=ok; exit 0 ;;
*systemd-run*) exit 0 ;;
esac
echo out
echo err >&2
exit ${CODE:-0}
`

// metalJSONSetup is a setup for a metal command: machine nuc1 registered, the
// stub ssh, sshpass and ansible-playbook in place. args are the command line.
func metalJSONSetup(args ...string) func(*testing.T) (string, []string) {
	return func(t *testing.T) (string, []string) {
		t.Setenv(config.TokenEnv, gateToken)
		t.Chdir(t.TempDir())
		dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
		testexec.Stub(t, map[string]string{
			"ssh":              metalStub,
			"sshpass":          "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n",
			"ansible-playbook": "#!/bin/sh\necho 'PLAY RECAP stub'\nexit 0\n",
		})
		if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Kind: vmtool.MachineMetal, Host: "192.0.2.7"}); err != nil {
			t.Fatal(err)
		}
		installNoVMs(t)
		return dir, args
	}
}

// config host: the three words of the human line, named; port is the daemon's
// default when the host has none.
func TestJSONConfigHost(t *testing.T) {
	dir, _ := configJSONDir()(t)
	r, o := runJSON(t, dir, "vmtool config host", "config", "host", "veritech01")
	if r.code != 0 || o["name"] != "veritech01" || o["address"] != "v.labtwo" || o["ssh_user"] != "ubuntu" || o["port"] != float64(config.DefaultPort) || len(o) != 4 {
		t.Errorf("exit %d: %v %s", r.code, o, r.errOut)
	}
	r, o = runJSON(t, dir, "vmtool config host-vms", "config", "host-vms", "veritech01")
	if r.code != 0 || o["name"] != "veritech01" || o["vms"] != true || len(o) != 2 {
		t.Errorf("host-vms: exit %d: %v %s", r.code, o, r.errOut)
	}
	r = runContract(t, dir, "vmtool config host", "--json", "config", "host", "nosuch")
	if r.code != 1 || r.out != "" || !strings.Contains(r.errOut, "unknown host") {
		t.Errorf("unknown host: exit %d, out %q, stderr %q", r.code, r.out, r.errOut)
	}
}

// server-url: the URL, or an empty one when there is no store.
func TestJSONConfigServerURL(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	r, o := runJSON(t, dir, "vmtool config server-url", "config", "server-url")
	if r.code != 0 || o["url"] != s.ts.URL || len(o) != 1 {
		t.Errorf("with a store: exit %d: %v", r.code, o)
	}
	dir, _ = configJSONDir()(t)
	r, o = runJSON(t, dir, "vmtool config server-url", "config", "server-url")
	if r.code != 0 || o["url"] != "" || len(o) != 1 {
		t.Errorf("no store: exit %d: %v", r.code, o)
	}
}

// set-host: the host and the file; the file really changed.
func TestJSONConfigSetHost(t *testing.T) {
	dir, _ := configJSONDir()(t)
	r, o := runJSON(t, dir, "vmtool config set-host", "config", "set-host", "veritech02", "--address", "192.0.2.9", "--ssh-user", "ubuntu")
	if r.code != 0 || o["host"] != "veritech02" || o["file"] != config.FileName || len(o) != 2 {
		t.Fatalf("exit %d: %v %s", r.code, o, r.errOut)
	}
	c, err := config.Load(dir)
	if err != nil || c.Hosts["veritech02"].Address != "192.0.2.9" {
		t.Errorf("config not written: %v %+v", err, c.Hosts)
	}
	// The lone --vms form says the same.
	r, o = runJSON(t, dir, "vmtool config set-host", "config", "set-host", "veritech02", "--vms=false")
	if r.code != 0 || o["host"] != "veritech02" || o["file"] != config.FileName {
		t.Errorf("--vms: exit %d: %v %s", r.code, o, r.errOut)
	}
}

// write: {wrote}; neither the token nor its hash is on either stream.
func TestJSONConfigWriteNoSecret(t *testing.T) {
	const tok = "placeholder-token-value"
	t.Setenv(config.TokenEnv, tok)
	dir := t.TempDir()
	r, o := runJSON(t, dir, "vmtool config write", "config", "write", "--name", "ws")
	if r.code != 0 || o["wrote"] != config.FileName || len(o) != 1 {
		t.Fatalf("exit %d: %v %s", r.code, o, r.errOut)
	}
	for _, s := range []string{r.out, r.errOut, r.leak} {
		if strings.Contains(s, tok) || strings.Contains(s, config.Hash(tok)) {
			t.Errorf("the token or its hash is printed: %q", s)
		}
	}
	if c, err := config.Load(dir); err != nil || c.TokenSHA256 != config.Hash(tok) {
		t.Errorf("config: %v %+v", err, c)
	}
}

// metal list and info: the API's MachineInfo.
func TestJSONMetalListInfo(t *testing.T) {
	dir, _ := metalJSONSetup()(t)
	r, o := runJSON(t, dir, "vmtool metal list", "metal", "list")
	if r.code != 0 {
		t.Fatalf("list: exit %d: %s", r.code, r.errOut)
	}
	ms := asList(t, o, "machines")
	if len(ms) != 1 {
		t.Fatalf("machines: %v", ms)
	}
	want := func(m map[string]any) bool {
		return m["name"] == "nuc1" && m["kind"] == "metal" && m["host"] == "192.0.2.7" && m["state"] == "reachable" && m["os"] == "Ubuntu 24.04 LTS"
	}
	if !want(ms[0].(map[string]any)) {
		t.Errorf("list row: %v", ms[0])
	}
	r, o = runJSON(t, dir, "vmtool metal info", "metal", "info", "nuc1")
	if r.code != 0 || !want(o) {
		t.Errorf("info: exit %d: %v %s", r.code, o, r.errOut)
	}
	if _, ok := o["error"]; ok {
		t.Errorf("info has an error key for a reachable machine: %v", o)
	}
}

// With no machine, the list is an empty array, not null.
func TestJSONMetalListEmpty(t *testing.T) {
	dir, _ := metalJSONSetup()(t)
	if err := os.RemoveAll(filepath.Join(dir, ".machines")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(".machines"); err != nil {
		t.Fatal(err)
	}
	r, o := runJSON(t, dir, "vmtool metal list", "metal", "list")
	if r.code != 0 || len(asList(t, o, "machines")) != 0 || !strings.Contains(r.out, `"machines": []`) {
		t.Errorf("exit %d: %q", r.code, r.out)
	}
}

// rm, reboot and playbook.
func TestJSONMetalRmRebootPlaybook(t *testing.T) {
	dir, _ := metalJSONSetup()(t)
	r, o := runJSON(t, dir, "vmtool metal reboot", "metal", "reboot", "nuc1")
	if r.code != 0 || o["name"] != "nuc1" || o["state"] != "rebooting" || o["kind"] != "metal" || o["host"] != "192.0.2.7" {
		t.Errorf("reboot: exit %d: %v %s", r.code, o, r.errOut)
	}
	r, o = runJSON(t, dir, "vmtool metal playbook", "metal", "playbook", "nuc1", "site.yml")
	if r.code != 0 || o["machine"] != "nuc1" || o["playbook"] != "site.yml" || o["completed"] != true || len(o) != 3 {
		t.Errorf("playbook: exit %d: %v %s", r.code, o, r.errOut)
	}
	if !strings.Contains(r.errOut, "PLAY RECAP stub") {
		t.Errorf("ansible's output is not on stderr: %q", r.errOut)
	}
	if strings.Contains(r.out, "PLAY RECAP") {
		t.Errorf("ansible's output is on stdout: %q", r.out)
	}
	r, o = runJSON(t, dir, "vmtool metal rm", "metal", "rm", "nuc1")
	if r.code != 0 || o["removed"] != "nuc1" || len(o) != 1 {
		t.Errorf("rm: exit %d: %v %s", r.code, o, r.errOut)
	}
	if _, err := vmtool.LoadMachine("nuc1"); err == nil {
		t.Error("rm left the machine registered")
	}
}

// add: the MachineInfo; ansible's output and the progress go to stderr; no
// password or key is on either stream.
func TestJSONMetalAdd(t *testing.T) {
	dir, _ := metalJSONSetup()(t)
	r, o := runJSON(t, dir, "vmtool metal add", "metal", "add", "nuc2", "192.0.2.8", "--kind", "alien")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	if o["name"] != "nuc2" || o["kind"] != "alien" || o["host"] != "192.0.2.8" || o["state"] != "reachable" || o["os"] != "Ubuntu 24.04 LTS" {
		t.Errorf("add: %v", o)
	}
	if !strings.Contains(r.errOut, "PLAY RECAP stub") || !strings.Contains(r.errOut, "checking ssh") {
		t.Errorf("progress is not on stderr: %q", r.errOut)
	}
	for _, s := range []string{r.out, r.errOut, r.leak} {
		for _, secret := range []string{metalPass, metalKey, "packer/packer"} {
			if strings.Contains(s, secret) {
				t.Errorf("%q is printed: %q", secret, s)
			}
		}
	}
	// The human form is untouched: the old line on stdout.
	dir, _ = metalJSONSetup()(t)
	r = runContract(t, dir, "vmtool metal add", "metal", "add", "nuc3", "192.0.2.9")
	if r.code != 0 || !strings.Contains(r.leak, `metal "nuc3" onboarded at 192.0.2.9 (Ubuntu 24.04 LTS)`) {
		t.Errorf("human add: exit %d, leak %q, stderr %q", r.code, r.leak, r.errOut)
	}
}

// exec: the program's streams and status are the object, and the status is
// still the exit code; -t is a usage error.
func TestJSONMetalExec(t *testing.T) {
	dir, _ := metalJSONSetup()(t)
	r, o := runJSON(t, dir, "vmtool metal exec", "metal", "exec", "nuc1", "--", "echo", "hi")
	if r.code != 0 || o["exit_code"] != float64(0) || o["stdout"] != "out\n" || o["stderr"] != "err\n" || len(o) != 3 {
		t.Errorf("exit %d: %v %s", r.code, o, r.errOut)
	}
	if r.errOut != "" {
		t.Errorf("stderr %q: the program's stderr belongs in the object", r.errOut)
	}
	t.Setenv("CODE", "7")
	r, o = runJSON(t, dir, "vmtool metal exec", "metal", "exec", "nuc1", "--", "false")
	if r.code != 7 || o["exit_code"] != float64(7) || o["stdout"] != "out\n" {
		t.Errorf("exit %d: %v", r.code, o)
	}
	r = runContract(t, dir, "vmtool metal exec", "--json", "metal", "exec", "-t", "nuc1", "--", "top")
	if r.code != 2 || r.out != "" || !strings.Contains(r.errOut, `"bad_request"`) || !strings.Contains(r.errOut, "--tty") {
		t.Errorf("-t: exit %d, out %q, stderr %q", r.code, r.out, r.errOut)
	}
	// After the target, --json belongs to the remote command.
	r = runContract(t, dir, "vmtool metal exec", "metal", "exec", "nuc1", "ls", "--json")
	if r.code != 7 || r.out != "" || r.leak != "out\n" {
		t.Errorf("remote --json: exit %d, out %q, leak %q", r.code, r.out, r.leak)
	}
}

// metal ssh is interactive: --json is refused with exit 2.
func TestJSONMetalSSHInteractive(t *testing.T) {
	root, _ := rootFor(t.TempDir(), nil, nil)
	c, _, err := root.Find([]string{"metal", "ssh"})
	if err != nil || c.Annotations[annotationInteractive] != "true" {
		t.Fatalf("metal ssh is not annotated interactive: %v %v", c, err)
	}
}

// noVMs is a local libvirt that has no VM of any name, so metal add finds the
// machine's name free.
type noVMs struct{ jsonFake }

func (noVMs) Info(n string) (*vmtool.VMInfo, error) {
	return nil, fmt.Errorf("VM %q: %w", n, vmtool.ErrNotFound)
}

func installNoVMs(t *testing.T) {
	t.Helper()
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
			return noVMs{newJSONFake(e.cfg.Name)}, func() {}, nil
		})
	}
}
