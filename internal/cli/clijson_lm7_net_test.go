package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/store"
	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/store/db"
	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/internal/tokenhash"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// LM7: --json for the net commands. attach, create, delete, detach and show
// run against the jsonFake host; join and leave (and a registered delete) need
// a registry, so they run against the real store, in process, and hosts that
// keep their networks and links.

func init() {
	jsonCases["vmtool net create"] = jsonCase1("net", "create", "veritech01", "net1")
	jsonCases["vmtool net show"] = jsonCase1("net", "show", "veritech01", "net1")
	jsonCases["vmtool net delete"] = jsonCase1("net", "delete", "veritech01", "net1")
	jsonCases["vmtool net attach"] = jsonCase1("net", "attach", "veritech01/vm1", "net1", "--ip", "10.77.1.50")
	jsonCases["vmtool net detach"] = jsonCase1("net", "detach", "veritech01/vm1", "net1")
	jsonCases["vmtool net join"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		sp := newSpanCLI(t)
		sp.run(t, "net", "create", "veritech01", "net1")
		return sp.dir, []string{"net", "join", "veritech02", "net1"}
	}}
	jsonCases["vmtool net leave"] = jsonCase{setup: func(t *testing.T) (string, []string) {
		sp := newSpanCLI(t)
		sp.run(t, "net", "create", "veritech01", "net1")
		sp.run(t, "net", "join", "veritech02", "net1")
		return sp.dir, []string{"net", "leave", "veritech02", "net1"}
	}}
}

// spanState is every host's libvirt networks, shared by the hosts' Managers.
type spanState struct {
	mu    sync.Mutex
	nets  map[string]map[string]*vmtool.NetworkInfo // host -> name -> network
	links map[string]string
}

// spanMgr is one host: the jsonFake with networks of its own and the member
// primitives (app.NetMember).
type spanMgr struct {
	jsonFake
	name string
	ip   string
	st   *spanState
}

func (m spanMgr) get(name string) *vmtool.NetworkInfo {
	m.st.mu.Lock()
	defer m.st.mu.Unlock()
	return m.st.nets[m.name][name]
}

func (m spanMgr) put(n *vmtool.NetworkInfo) {
	m.st.mu.Lock()
	defer m.st.mu.Unlock()
	if m.st.nets[m.name] == nil {
		m.st.nets[m.name] = map[string]*vmtool.NetworkInfo{}
	}
	m.st.nets[m.name][n.Name] = n
}

func (m spanMgr) Network(name string) (*vmtool.NetworkInfo, error) {
	if n := m.get(name); n != nil {
		c := *n
		return &c, nil
	}
	return nil, fmt.Errorf("network %q: %w", name, vmtool.ErrNotFound)
}

func (m spanMgr) CreateNetworkVNI(name, subnet, domain string, vni uint32) (*vmtool.NetworkInfo, error) {
	pre := strings.TrimSuffix(subnet, "0/24")
	n := &vmtool.NetworkInfo{Name: name, Bridge: vmtool.BridgeName(vni), VNI: vni, Vmtool: true, Forward: "nat", Subnet: subnet,
		Domain: domain, MTU: 1450, Gateway: pre + "1", DHCPStart: pre + "100", DHCPEnd: pre + "200", Active: true, Autostart: true, VMs: []string{}}
	m.put(n)
	c := *n
	return &c, nil
}

func (m spanMgr) CreateNetworkMember(name string, vni uint32) (*vmtool.NetworkInfo, error) {
	n := &vmtool.NetworkInfo{Name: name, Bridge: vmtool.BridgeName(vni), VNI: vni, Vmtool: true, MTU: 1450, Active: true, Autostart: true, VMs: []string{}}
	m.put(n)
	c := *n
	return &c, nil
}

func (m spanMgr) SetNetworkLinks(name, ip, dev string, peers []string) error {
	m.st.mu.Lock()
	defer m.st.mu.Unlock()
	m.st.links[m.name+"/"+name] = ip + " " + dev + " " + strings.Join(peers, ",")
	return nil
}

func (m spanMgr) RemoveNetworkLink(name string) error {
	m.st.mu.Lock()
	defer m.st.mu.Unlock()
	delete(m.st.links, m.name+"/"+name)
	return nil
}

func (m spanMgr) Underlay(string) (*vmtool.Underlay, error) {
	return &vmtool.Underlay{IP: m.ip, Dev: "eth-test"}, nil
}

func (m spanMgr) DeleteNetwork(name string) error {
	m.st.mu.Lock()
	defer m.st.mu.Unlock()
	if m.st.nets[m.name][name] == nil {
		return fmt.Errorf("network %q: %w", name, vmtool.ErrNotFound)
	}
	delete(m.st.nets[m.name], name)
	return nil
}

// spanCLI is a working directory whose config names the in-process store and
// two hosts, veritech01 (the home of net1) and veritech02.
type spanCLI struct {
	dir string
	st  *spanState
}

func newSpanCLI(t *testing.T) *spanCLI {
	t.Helper()
	const tok = "placeholder-store-token"
	data := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(data, "store.db"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c := &store.Config{Listen: "127.0.0.1:8095", Tokens: map[string]string{"alice": tokenhash.Hash(tok)}}
	var logs bytes.Buffer
	ts := httptest.NewTLSServer(store.NewServer(c, slog.New(slog.NewJSONHandler(&logs, nil)), store.Deps{Blob: blob.NewMemory(), DB: d}).Handler())
	t.Cleanup(ts.Close)
	old := storeClientOptions
	t.Cleanup(func() { storeClientOptions = old })
	storeClientOptions = []storeclient.Option{storeclient.WithRootCAs(ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs)}

	t.Setenv(config.TokenEnv, gateToken)
	t.Setenv(config.ServerTokenEnv, tok)
	t.Chdir(t.TempDir())
	cfg := "name: jetfire\ntoken_sha256: " + config.Hash(gateToken) + "\nserver:\n  url: " + ts.URL + "\n" +
		"hosts:\n  veritech01: { address: 192.0.2.11, ssh_user: ubuntu }\n  veritech02: { address: 192.0.2.12, ssh_user: ubuntu }\n"
	sp := &spanCLI{dir: writeWorkdir(t, cfg, ""), st: &spanState{nets: map[string]map[string]*vmtool.NetworkInfo{}, links: map[string]string{}}}

	oldR := newResolver
	t.Cleanup(func() { newResolver = oldR })
	ips := map[string]string{"veritech01": "192.0.2.11", "veritech02": "192.0.2.12", "jetfire": "192.0.2.10"}
	mk := func(host string) app.Manager {
		return spanMgr{jsonFake: newJSONFake(host), name: host, ip: ips[host], st: sp.st}
	}
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, []string{"veritech01", "veritech02"}, func() (app.Manager, func(), error) {
			return mk(e.cfg.Name), func() {}, nil
		}).WithRemote(func(host, _ string) (app.Manager, func(), error) {
			return mk(host), func() {}, nil
		})
	}
	return sp
}

// run runs one command in human mode and fails the test on an error.
func (sp *spanCLI) run(t *testing.T, args ...string) contractRun {
	t.Helper()
	r := runContract(t, sp.dir, "vmtool "+strings.Join(args[:2], " "), args...)
	if r.code != 0 {
		t.Fatalf("%v: exit %d: %s %v", args, r.code, r.errOut, r.err)
	}
	return r
}

// net create: the API's NetworkInfo, nothing else on stdout.
func TestJSONNetCreateShow(t *testing.T) {
	for _, c := range []string{"create", "show"} {
		dir := jsonDir(t, "")
		r, o := runJSON(t, dir, "vmtool net "+c, "net", c, "veritech01", "net1")
		if r.code != 0 {
			t.Fatalf("%s: exit %d: %s", c, r.code, r.errOut)
		}
		if o["name"] != "net1" || o["vmtool"] != true || o["vni"] != float64(1) || o["bridge"] != "vmt-000001" ||
			o["subnet"] != "10.77.1.0/24" || o["gateway"] != "10.77.1.1" || o["domain"] != "net1.test" ||
			o["mtu"] != float64(1450) || o["dhcp_start"] != "10.77.1.100" || o["dhcp_end"] != "10.77.1.200" ||
			o["forward"] != "nat" || o["active"] != true || o["autostart"] != true {
			t.Errorf("%s: %v", c, o)
		}
		if vms := asList(t, o, "vms"); len(vms) != 2 || vms[0] != "vm1" {
			t.Errorf("%s: vms %v", c, vms)
		}
		if _, ok := o["orphaned"]; ok {
			t.Errorf("%s: orphaned with none: %v", c, o)
		}
		if strings.Contains(r.errOut, "created") && c == "show" {
			t.Errorf("show wrote a human line: %q", r.errOut)
		}
	}
	// A network vmtool did not make has no vni.
	dir := jsonDir(t, "")
	r, o := runJSON(t, dir, "vmtool net show", "net", "show", "veritech01", "default")
	if r.code != 0 || o["name"] != "default" || o["vmtool"] != false {
		t.Fatalf("default: exit %d %v", r.code, o)
	}
	if _, ok := o["vni"]; ok {
		t.Errorf("a network vmtool did not make has a vni: %v", o)
	}
}

// net show lists the orphans it could not remove under orphaned.
func TestJSONNetShowOrphans(t *testing.T) {
	obj := netShowObject(&app.NetworkShow{Info: fakeNet("net1")})
	if obj.Orphaned != nil {
		t.Errorf("no orphans: %v", obj.Orphaned)
	}
	obj = netShowObject(&app.NetworkShow{Info: fakeNet("net1"), Orphans: []storeclient.NetworkReservation{
		{Network: "net1", Mac: "52:54:00:00:00:09", Ip: "10.77.1.9", Vm: "gone", Host: "veritech01"}}})
	if len(obj.Orphaned) != 1 || obj.Orphaned[0].Vm != "gone" || obj.Orphaned[0].Ip != "10.77.1.9" || obj.Orphaned[0].Host != "veritech01" {
		t.Errorf("orphans %+v", obj.Orphaned)
	}
}

// attach: the API's NetworkAttachment with the VM; detach: the VM and card.
func TestJSONNetAttachDetach(t *testing.T) {
	dir := jsonDir(t, "")
	r, o := runJSON(t, dir, "vmtool net attach", "net", "attach", "veritech01/vm1", "net2", "--ip", "10.77.1.50")
	if r.code != 0 {
		t.Fatalf("attach: exit %d: %s", r.code, r.errOut)
	}
	if o["vm"] != "veritech01/vm1" || o["network"] != "net2" || o["mac"] == "" || o["ip"] == nil {
		t.Errorf("attach: %v", o)
	}
	dir = jsonDir(t, "")
	r, o = runJSON(t, dir, "vmtool net detach", "net", "detach", "veritech01/vm1", "net1")
	if r.code != 0 {
		t.Fatalf("detach: exit %d: %s", r.code, r.errOut)
	}
	if o["vm"] != "veritech01/vm1" || o["detached"] != "net1" || len(o) != 2 {
		t.Errorf("detach: %v", o)
	}
}

// delete on a host with no registry: {network, host, registered false} and
// empty lists; the human text is unchanged without --json (TestNetDeleteAndShow).
func TestJSONNetDeleteLocal(t *testing.T) {
	dir := jsonDir(t, "")
	r, o := runJSON(t, dir, "vmtool net delete", "net", "delete", "veritech01", "net1")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.errOut)
	}
	if o["network"] != "net1" || o["host"] != "veritech01" || o["registered"] != false ||
		len(asList(t, o, "members")) != 0 || len(asList(t, o, "left")) != 0 {
		t.Errorf("delete: %v", o)
	}
}

// join, leave and a registered delete against the real store.
func TestJSONNetSpan(t *testing.T) {
	sp := newSpanCLI(t)
	sp.run(t, "net", "create", "veritech01", "net1")

	r, o := runJSON(t, sp.dir, "vmtool net join", "net", "join", "veritech02", "net1")
	if r.code != 0 {
		t.Fatalf("join: exit %d: %s", r.code, r.errOut)
	}
	if o["host"] != "veritech02" || o["network"] != "net1" || o["home"] != "veritech01" || o["vni"] != float64(10000) ||
		o["registered"] != false || o["lan_ip"] != "192.0.2.12" || o["lan_dev"] != "eth-test" {
		t.Errorf("join: %v", o)
	}
	if peers := asList(t, o, "peers"); len(peers) != 1 || peers[0] != "192.0.2.11" {
		t.Errorf("join peers: %v", peers)
	}
	if len(asList(t, o, "skipped")) != 0 {
		t.Errorf("join skipped: %v", o["skipped"])
	}

	r, o = runJSON(t, sp.dir, "vmtool net leave", "net", "leave", "veritech02", "net1")
	if r.code != 0 {
		t.Fatalf("leave: exit %d: %s", r.code, r.errOut)
	}
	if o["host"] != "veritech02" || o["network"] != "net1" || len(asList(t, o, "skipped")) != 0 || len(o) != 3 {
		t.Errorf("leave: %v", o)
	}

	sp.run(t, "net", "join", "veritech02", "net1")
	r, o = runJSON(t, sp.dir, "vmtool net delete", "net", "delete", "veritech01", "net1")
	if r.code != 0 {
		t.Fatalf("delete: exit %d: %s", r.code, r.errOut)
	}
	if o["network"] != "net1" || o["host"] != "veritech01" || o["registered"] != true {
		t.Errorf("delete: %v", o)
	}
	if m := asList(t, o, "members"); len(m) != 1 || m[0] != "veritech02" {
		t.Errorf("delete members: %v", m)
	}
	if len(asList(t, o, "left")) != 0 {
		t.Errorf("delete left: %v", o["left"])
	}
}

// A failure under --json is the error object on stderr and nothing on stdout.
func TestJSONNetFailure(t *testing.T) {
	dir := jsonDir(t, "")
	r := runContract(t, dir, "vmtool net show", "--json", "net", "show", "veritech01", "ghost")
	if r.code != 1 || r.out != "" || !strings.Contains(r.errOut, `"not_found"`) {
		t.Errorf("exit %d, out %q, stderr %q", r.code, r.out, r.errOut)
	}
}
