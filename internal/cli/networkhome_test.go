package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/testexec"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// homeConfig lists hv1 (holds VMs) and home1 (vms: false, a network home).
func homeConfig() string {
	return "name: jetfire\ntoken_sha256: " + config.Hash(gateToken) + "\nhosts:\n" +
		"  hv1: { address: 192.0.2.11, ssh_user: ubuntu }\n" +
		"  home1: { address: 192.0.2.13, ssh_user: ubuntu, vms: false }\n"
}

// installHomeHosts is the real resolver for the config (so the vms: switch
// comes from config.yml) with every remote host replaced by a fake.
func installHomeHosts(t *testing.T, mgrs map[string]listMgr) *createLog {
	t.Helper()
	log := &createLog{}
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		// NewWith, never New: the local connector is a fake, so no test here can
		// open libvirt. The network homes are what config.yml says.
		var names, homes []string
		for n, h := range e.cfg.Hosts {
			names = append(names, n)
			if !h.HoldsVMs() {
				homes = append(homes, n)
			}
		}
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, names, func() (app.Manager, func(), error) {
			return mgrs[e.cfg.Name], func() {}, nil
		}).WithNetworkHomes(homes...).WithRemote(func(host, _ string) (app.Manager, func(), error) {
			if m, ok := mgrs[host]; ok {
				return m, func() {}, nil
			}
			return createFake{log: log, ip: "192.0.2.50"}, func() {}, nil
		})
	}
	return log
}

// create on a network home is refused with a plain message, and nothing is
// created there.
func TestCreateOnNetworkHomeRefused(t *testing.T) {
	testexec.Ansible(t)
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	log := installHomeHosts(t, map[string]listMgr{"jetfire": {}})
	dir := writeWorkdir(t, homeConfig(), "")
	var out bytes.Buffer
	root, e := rootFor(dir, &out, io.Discard)
	e.tunnelExe = stubTunnelExe
	root.SetArgs([]string{"create", "home1/web", "u.qcow2", "--inventory", ""})
	err := root.Execute()
	if err == nil {
		t.Fatal("create on a network home succeeded")
	}
	for _, w := range []string{"home1", "network home", "vms: false"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q lacks %q", err, w)
		}
	}
	for _, op := range []string{"Define", "Start", "WaitForIP"} {
		if log.has(op) {
			t.Errorf("%s ran on the network home", op)
		}
	}
}

// `vmtool list` shows a network home as one row that says so (it has no VMs to
// list), next to the hosts that do.
func TestListShowsNetworkHome(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, homeConfig(), "")
	installHomeHosts(t, map[string]listMgr{
		"jetfire": {},
		"hv1":     {vms: []vmtool.VMInfo{{Name: "db", State: vmtool.StateRunning, VCPUs: 1, Memory: 2}}},
		"home1":   {},
	})
	r := runGate(t, dir, map[string]bool{"vmtool list": true}, "list")
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := `NAME STATE VCPUS MEMORY AUTOSTART IP
home1 network home: runs no VMs
hv1/db running 1 2 GiB off -
`
	if got := norm(r.out); got != want {
		t.Errorf("list output:\n%s\nwant:\n%s", got, want)
	}
}

// `vmtool hosts` marks the network home in its NAME cell.
func TestHostsShowsNetworkHome(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, homeConfig(), "")
	installHomeHosts(t, map[string]listMgr{
		"jetfire": {},
		"hv1":     {st: app.HostStatus{Address: "192.0.2.11:9473", Reachable: true, APIVersion: "1.0.0"}},
		"home1":   {st: app.HostStatus{Address: "192.0.2.13:9473", Reachable: true, APIVersion: "1.0.0"}},
	})
	r := runGate(t, dir, map[string]bool{"vmtool hosts": true}, "hosts")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.Contains(norm(r.out), "home1 (network home) 192.0.2.13:9473 yes 1.0.0") {
		t.Errorf("hosts output lacks the network-home mark:\n%s", r.out)
	}
	if strings.Contains(r.out, "hv1 (network home)") {
		t.Errorf("hv1 marked as a network home:\n%s", r.out)
	}
}

// An all-hosts `list` does not ask a network home (its fake would fail): one
// row says so. Named explicitly, the home is asked and its failure is the
// command's error.
func TestListAllHostsDoesNotAskNetworkHome(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, homeConfig(), "")
	installHomeHosts(t, map[string]listMgr{
		"jetfire": {},
		"hv1":     {vms: []vmtool.VMInfo{{Name: "db", State: vmtool.StateRunning, VCPUs: 1, Memory: 2}}},
		"home1":   {err: errors.New("home1 unreachable")},
	})
	r := runGate(t, dir, map[string]bool{"vmtool list": true}, "list")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if strings.Contains(r.out, "ERROR") || !strings.Contains(norm(r.out), "home1 network home: runs no VMs") {
		t.Errorf("all-hosts list:\n%s", r.out)
	}
	r = runGate(t, dir, map[string]bool{"vmtool list": true}, "list", "home1")
	if r.err == nil || !strings.Contains(r.err.Error(), "home1 unreachable") {
		t.Errorf("list home1: err = %v, want the home's failure", r.err)
	}
}

// installHomeTemplateHosts is installTemplateHosts with home1 a network home.
func installHomeTemplateHosts(t *testing.T, mgrs map[string]app.Manager) {
	t.Helper()
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, []string{"hv1", "home1"}, func() (app.Manager, func(), error) {
			return mgrs[e.cfg.Name], func() {}, nil
		}).WithNetworkHomes("home1").WithRemote(func(host, _ string) (app.Manager, func(), error) {
			return mgrs[host], func() {}, nil
		})
	}
}

// Same pair for `templates list`: all hosts leaves the home alone, --host
// asks it.
func TestTemplatesListDoesNotAskNetworkHome(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, homeConfig(), "")
	mu := &sync.Mutex{}
	calls := &[]string{}
	installHomeTemplateHosts(t, map[string]app.Manager{
		"jetfire": listMgr{},
		"hv1":     tplHost{mu: mu, calls: calls, tpls: []app.TemplateInfo{tmpl("vmtpl-abababababab", "img1", "image", strings.Repeat("ab", 32), false, 2)}},
		"home1":   tplHost{mu: mu, calls: calls, err: errors.New("home1 unreachable")},
	})
	keep := map[string]bool{"vmtool templates list": true}
	r := runGate(t, dir, keep, "templates", "list")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if strings.Contains(r.out, "ERROR") || !strings.Contains(norm(r.out), "home1 network home: runs no VMs") {
		t.Errorf("all-hosts templates list:\n%s", r.out)
	}
	r = runGate(t, dir, keep, "templates", "list", "--host", "home1")
	if r.err == nil || !strings.Contains(r.err.Error(), "home1 unreachable") {
		t.Errorf("templates list --host home1: err = %v, want the home's failure", r.err)
	}
}
