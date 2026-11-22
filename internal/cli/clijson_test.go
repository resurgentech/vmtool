package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/testexec"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// LM5: --json for the VM and host commands. The cases join jsonCases (the
// contract walk in clicontract_test.go); the focused tests pin each shape.

// jsonFake is a host on which every VM operation succeeds and Info answers
// with a running VM that has an address and a card on a vmtool network.
type jsonFake struct{ createFake }

func newJSONFake(host string) jsonFake {
	return jsonFake{createFake{cliFake: cliFake{host: host, f: &fakeHosts{}}, log: &createLog{}, ip: "10.0.0.5"}}
}

func (jsonFake) Stop(string) error                { return nil }
func (jsonFake) Resume(string) error              { return nil }
func (jsonFake) Reboot(string) error              { return nil }
func (jsonFake) SetAutostart(string, bool) error  { return nil }
func (jsonFake) ResizeDisk(string, uint) error    { return nil }
func (jsonFake) MigrateDisk(string, string) error { return nil }
func (jsonFake) AddDisk(string, uint, string) (*vmtool.DiskDevice, error) {
	return &vmtool.DiskDevice{Path: "/pool/web-2.qcow2", Target: "vdb"}, nil
}
func (jsonFake) Info(n string) (*vmtool.VMInfo, error) {
	return &vmtool.VMInfo{Name: n, State: vmtool.StateRunning, VCPUs: 2, Memory: 4, IP: "10.0.0.5",
		Autostart: true, Firmware: "efi", UUID: notesUUID,
		Networks: []vmtool.NetAttachment{{Network: "net1", MAC: "52:54:00:00:00:01", IP: "10.77.0.5"}}}, nil
}

// installJSONFake makes every host in the config the jsonFake.
func installJSONFake(t *testing.T) {
	t.Helper()
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		var configured []string
		for n := range e.cfg.Hosts {
			configured = append(configured, n)
		}
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, configured, func() (app.Manager, func(), error) {
			return newJSONFake(e.cfg.Name), func() {}, nil
		}).WithRemote(func(host, _ string) (app.Manager, func(), error) {
			return newJSONFake(host), func() {}, nil
		})
	}
}

// jsonDir is a working directory for the fake host "jetfire", with stub
// ansible and ssh and the fake installed. extra is more config.yml text.
func jsonDir(t *testing.T, extra string) string {
	t.Helper()
	testexec.Ansible(t)
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	installJSONFake(t)
	return writeWorkdir(t, "name: jetfire\n"+extra+goodConfig(), "")
}

func jsonCase1(args ...string) jsonCase {
	return jsonCase{setup: func(t *testing.T) (string, []string) { return jsonDir(t, ""), args }}
}

// sshStub installs a stub ssh (and sshpass) that prints "out" on stdout and
// "err" on stderr and exits with code.
func sshStub(t *testing.T, code string) {
	t.Helper()
	testexec.Stub(t, map[string]string{
		"ssh":     "#!/bin/sh\necho out\necho err >&2\nexit " + code + "\n",
		"sshpass": "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n",
	})
}

func init() {
	for _, c := range []string{"start", "stop", "resume", "poweroff", "reboot", "info"} {
		jsonCases["vmtool "+c] = jsonCase1(c, "jetfire/web")
	}
	jsonCases["vmtool create"] = jsonCase1("create", "jetfire/web", "u.qcow2", "--inventory", "")
	jsonCases["vmtool delete"] = jsonCase1("delete", "jetfire/web")
	jsonCases["vmtool autostart"] = jsonCase1("autostart", "jetfire/web", "on")
	jsonCases["vmtool networks"] = jsonCase1("networks")
	jsonCases["vmtool bridges"] = jsonCase1("bridges")
	jsonCases["vmtool playbook"] = jsonCase1("playbook", "jetfire/web", "site.yml")
	jsonCases["vmtool migrate-disk"] = jsonCase1("migrate-disk", "jetfire/web", "fast")
	jsonCases["vmtool resize-disk"] = jsonCase1("resize-disk", "jetfire/web", "30")
	jsonCases["vmtool add-disk"] = jsonCase1("add-disk", "jetfire/web", "50")
	jsonCases["vmtool ssh-config"] = jsonCase1("ssh-config")
	jsonCases["vmtool exec"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		dir := jsonDir(t, "")
		sshStub(t, "0")
		return dir, []string{"exec", "jetfire/web", "--", "echo", "hi"}
	}}
	jsonCases["vmtool comment"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		s := newCLIStore(t)
		return storeWorkdir(t, s, storeTok), []string{"comment", "ws/web", "placeholder comment"}
	}}
	jsonCases["vmtool list"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		t.Setenv(config.TokenEnv, gateToken)
		t.Chdir(t.TempDir())
		installListHosts(t, map[string]listMgr{"jetfire": {vms: []vmtool.VMInfo{{Name: "web", State: vmtool.StateRunning}}}})
		return writeWorkdir(t, "name: jetfire\ntoken_sha256: "+config.Hash(gateToken)+"\n", ""), []string{"list"}
	}}
	jsonCases["vmtool hosts"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		t.Setenv(config.TokenEnv, gateToken)
		t.Chdir(t.TempDir())
		installListHosts(t, map[string]listMgr{"jetfire": {}})
		return writeWorkdir(t, "name: jetfire\ntoken_sha256: "+config.Hash(gateToken)+"\n", ""), []string{"hosts"}
	}}
}

// runJSON runs one command with --json against dir and decodes its stdout.
func runJSON(t *testing.T, dir, path string, args ...string) (contractRun, map[string]any) {
	t.Helper()
	r := runContract(t, dir, path, append([]string{"--json"}, args...)...)
	var obj map[string]any
	if r.out != "" {
		if err := json.Unmarshal([]byte(r.out), &obj); err != nil {
			t.Fatalf("%v: stdout is not an object: %v\n%s", args, err, r.out)
		}
	}
	return r, obj
}

// A VM command's result is the API's VMInfo: the same keys, the card's keys
// in the API's spelling (mac, ip, network), no field the API lacks.
func TestJSONVMShape(t *testing.T) {
	for _, c := range []string{"start", "stop", "resume", "poweroff", "reboot", "info"} {
		dir := jsonDir(t, "")
		r, o := runJSON(t, dir, "vmtool "+c, c, "jetfire/web")
		if r.code != 0 {
			t.Fatalf("%s: exit %d: %s", c, r.code, r.errOut)
		}
		if o["name"] != "web" || o["state"] != "running" || o["vcpus"] != float64(2) || o["memory"] != float64(4) ||
			o["ip"] != "10.0.0.5" || o["autostart"] != true || o["firmware"] != "efi" || o["uuid"] != notesUUID {
			t.Errorf("%s: %v", c, o)
		}
		nets, _ := o["networks"].([]any)
		if len(nets) != 1 {
			t.Fatalf("%s: networks %v", c, o["networks"])
		}
		card := nets[0].(map[string]any)
		if card["network"] != "net1" || card["mac"] != "52:54:00:00:00:01" || card["ip"] != "10.77.0.5" {
			t.Errorf("%s: card %v", c, card)
		}
		if r.errOut != "" {
			t.Errorf("%s: stderr %q", c, r.errOut)
		}
	}
	// autostart answers the VM too.
	_, o := runJSON(t, jsonDir(t, ""), "vmtool autostart", "autostart", "jetfire/web", "off")
	if o["name"] != "web" || o["state"] != "running" {
		t.Errorf("autostart: %v", o)
	}
}

func TestJSONDeleteShape(t *testing.T) {
	_, o := runJSON(t, jsonDir(t, ""), "vmtool delete", "delete", "jetfire/web")
	if len(o) != 1 || o["deleted"] != "jetfire/web" {
		t.Errorf("delete: %v", o)
	}
}

// create: the VM is the object; its progress, the closing timings line and
// the human lines go to stderr.
func TestJSONCreate(t *testing.T) {
	r, o := runJSON(t, jsonDir(t, ""), "vmtool create", "create", "jetfire/web", "u.qcow2", "--inventory", "")
	if r.code != 0 || o["name"] != "web" || o["ip"] != "10.0.0.5" {
		t.Fatalf("exit %d: %v\n%s", r.code, o, r.errOut)
	}
	if strings.Contains(r.out, "timings:") || strings.Contains(r.out, "created and started") {
		t.Errorf("human text on stdout:\n%s", r.out)
	}
	if !strings.Contains(r.errOut, "timings:") || !strings.Contains(r.errOut, `VM "jetfire/web" created and started`) {
		t.Errorf("stderr lacks the progress:\n%s", r.errOut)
	}
}

// A failed create in JSON mode: nothing on stdout, the "exists" line and the
// timings on stderr, then the one error object last.
func TestJSONCreateFailure(t *testing.T) {
	dir := jsonDir(t, "")
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith("jetfire", config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
			f := newJSONFake("jetfire")
			f.ip = ""
			return noIPFake{f}, func() {}, nil
		})
	}
	r := runContract(t, dir, "vmtool create", "--json", "create", "jetfire/web", "u.qcow2", "--inventory", "")
	if r.code != 1 || r.out != "" || r.leak != "" {
		t.Fatalf("exit %d out %q leak %q\n%s", r.code, r.out, r.leak, r.errOut)
	}
	if !strings.Contains(r.errOut, `exists and is running, but create did not finish`) || !strings.Contains(r.errOut, "timings:") ||
		!strings.Contains(r.errOut, `{"error":`) {
		t.Errorf("stderr:\n%s", r.errOut)
	}
}

// noIPFake is a host whose VM never gets an address.
type noIPFake struct{ jsonFake }

func (noIPFake) Info(n string) (*vmtool.VMInfo, error) {
	return &vmtool.VMInfo{Name: n, State: vmtool.StateRunning}, nil
}

func TestJSONComment(t *testing.T) {
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	_, o := runJSON(t, dir, "vmtool comment", "comment", "ws/web", "placeholder comment")
	if o["host"] != "ws" || o["name"] != "web" || o["comment"] != "placeholder comment" || o["uuid"] == nil {
		t.Errorf("comment: %v", o)
	}
}

func TestJSONListShape(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, twoHostCfg("  veritech02: { address: v2.labtwo, ssh_user: ubuntu }\n"), "")
	installListHosts(t, map[string]listMgr{
		"jetfire":    {vms: []vmtool.VMInfo{{Name: "web", State: vmtool.StateRunning, VCPUs: 2, Memory: 4, IP: "10.0.0.5"}}},
		"veritech01": {vms: []vmtool.VMInfo{{Name: "db", State: vmtool.StateShutoff, Autostart: true}}},
		"veritech02": {err: errors.New("libvirt:\nconnection refused")},
	})
	r, o := runJSON(t, dir, "vmtool list", "list")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	vms, _ := o["vms"].([]any)
	if len(vms) != 2 {
		t.Fatalf("vms %v", o["vms"])
	}
	web := vms[0].(map[string]any)
	if web["host"] != "jetfire" || web["name"] != "web" || web["state"] != "running" || web["ip"] != "10.0.0.5" {
		t.Errorf("row %v", web)
	}
	errs, _ := o["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors %v", o["errors"])
	}
	row := errs[0].(map[string]any)
	if row["host"] != "veritech02" || row["error"] != "libvirt: connection refused" {
		t.Errorf("error row %v", row)
	}
	// Nothing to list is an empty list, not a missing key.
	dir = writeWorkdir(t, "name: jetfire\ntoken_sha256: "+config.Hash(gateToken)+"\n", "")
	installListHosts(t, map[string]listMgr{"jetfire": {}})
	_, o = runJSON(t, dir, "vmtool list", "list")
	if v, ok := o["vms"].([]any); !ok || len(v) != 0 {
		t.Errorf("empty list: %v", o)
	}
}

func TestJSONHostsShape(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, twoHostCfg(""), "")
	installListHosts(t, map[string]listMgr{
		"jetfire":    {},
		"veritech01": {st: app.HostStatus{Address: "v.labtwo:9473", Err: errors.New("dial tcp: refused")}},
	})
	r, o := runJSON(t, dir, "vmtool hosts", "hosts")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	hs, _ := o["hosts"].([]any)
	if len(hs) != 2 {
		t.Fatalf("hosts %v", o["hosts"])
	}
	v := hs[1].(map[string]any)
	if v["name"] != "veritech01" || v["address"] != "v.labtwo:9473" || v["reachable"] != false || v["error"] != "dial tcp: refused" {
		t.Errorf("row %v", v)
	}
	if _, ok := o["store"]; ok {
		t.Errorf("a store row with no store configured: %v", o["store"])
	}
}

func TestJSONNetworksBridges(t *testing.T) {
	dir := jsonDir(t, "")
	_, o := runJSON(t, dir, "vmtool networks", "networks")
	nets, _ := o["networks"].([]any)
	if len(nets) != 1 || nets[0].(map[string]any)["name"] != "jetfire-net" {
		t.Errorf("networks %v", o)
	}
	_, o = runJSON(t, dir, "vmtool bridges", "bridges")
	brs, _ := o["bridges"].([]any)
	if len(brs) != 1 || brs[0] != "jetfire-br" {
		t.Errorf("bridges %v", o)
	}
}

func TestJSONDiskShapes(t *testing.T) {
	dir := jsonDir(t, "")
	_, o := runJSON(t, dir, "vmtool migrate-disk", "migrate-disk", "jetfire/web", "fast")
	if o["name"] != "web" || o["state"] != "running" {
		t.Errorf("migrate-disk: %v", o)
	}
	r, o := runJSON(t, dir, "vmtool resize-disk", "resize-disk", "jetfire/web", "30")
	vm, _ := o["vm"].(map[string]any)
	if vm["name"] != "web" || o["filesystem_grown"] != true {
		t.Errorf("resize-disk: %v\n%s", o, r.errOut)
	}
	_, o = runJSON(t, dir, "vmtool add-disk", "add-disk", "jetfire/web", "50")
	vm, _ = o["vm"].(map[string]any)
	if o["path"] != "/pool/web-2.qcow2" || o["target"] != "vdb" || vm["name"] != "web" {
		t.Errorf("add-disk: %v", o)
	}
}

// A grow that failed leaves the resize done: the object carries grow_error,
// and the command still fails (as without --json).
func TestJSONResizeGrowError(t *testing.T) {
	dir := jsonDir(t, "")
	t.Setenv("STUB_GROW_FAIL", "1")
	t.Setenv("STUB_GROW_OUT", "growpart: boom")
	r, o := runJSON(t, dir, "vmtool resize-disk", "resize-disk", "jetfire/web", "30")
	if r.code != 1 || o["filesystem_grown"] != false || !strings.Contains(o["grow_error"].(string), "boom") {
		t.Fatalf("exit %d: %v\n%s", r.code, o, r.errOut)
	}
	if !strings.Contains(r.errOut, `{"error":`) {
		t.Errorf("no error object:\n%s", r.errOut)
	}
}

// The ansible output goes to stderr, the object to stdout.
func TestJSONPlaybook(t *testing.T) {
	dir := jsonDir(t, "")
	testexec.Stub(t, map[string]string{
		"ansible-playbook": "#!/bin/sh\necho 'PLAY RECAP stub'\nexit 0\n",
		"ansible":          "#!/bin/sh\nexit 0\n",
		"ssh":              "#!/bin/sh\nexit 0\n",
		"sshpass":          "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n",
	})
	r, o := runJSON(t, dir, "vmtool playbook", "playbook", "jetfire/web", "site.yml")
	if r.code != 0 || o["vm"] != "jetfire/web" || o["playbook"] != "site.yml" || o["completed"] != true {
		t.Fatalf("exit %d: %v\n%s", r.code, o, r.errOut)
	}
	if strings.Contains(r.out, "PLAY RECAP") || !strings.Contains(r.errOut, "PLAY RECAP stub") {
		t.Errorf("stdout %q stderr %q", r.out, r.errOut)
	}
}

func TestJSONSSHConfig(t *testing.T) {
	r, o := runJSON(t, jsonDir(t, ""), "vmtool ssh-config", "ssh-config")
	cfg, _ := o["config"].(string)
	if r.code != 0 || !strings.HasPrefix(cfg, "Host *.vmtool\n") {
		t.Errorf("exit %d: %v", r.code, o)
	}
}

// exec: captured streams, the guest's exit code reported and returned.
func TestJSONExec(t *testing.T) {
	dir := jsonDir(t, "")
	sshStub(t, "3")
	r, o := runJSON(t, dir, "vmtool exec", "exec", "jetfire/web", "--", "false")
	if r.code != 3 {
		t.Fatalf("exit %d, want the guest's 3: %s", r.code, r.errOut)
	}
	if o["exit_code"] != float64(3) || o["stdout"] != "out\n" || o["stderr"] != "err\n" || len(o) != 3 {
		t.Errorf("exec: %v", o)
	}
	if r.errOut != "" || r.leak != "" {
		t.Errorf("stderr %q leak %q", r.errOut, r.leak)
	}
}

func TestJSONExecTTYRefused(t *testing.T) {
	dir := jsonDir(t, "")
	sshStub(t, "0")
	r := runContract(t, dir, "vmtool exec", "--json", "exec", "-t", "jetfire/web", "--", "top")
	if r.code != 2 || r.out != "" || !strings.Contains(r.errOut, `"bad_request"`) {
		t.Errorf("exit %d out %q stderr %q", r.code, r.out, r.errOut)
	}
}

// The interactive four refuse --json with exit 2 (TestCLIContract runs them).
func TestInteractiveMarked(t *testing.T) {
	root, _ := rootFor(t.TempDir(), nil, nil)
	for _, name := range []string{"ssh", "i", "server", "tunnel"} {
		c, _, err := root.Find([]string{name})
		if err != nil || c.Annotations[annotationInteractive] != "true" {
			t.Errorf("%s is not marked interactive (%v)", name, err)
		}
	}
}
