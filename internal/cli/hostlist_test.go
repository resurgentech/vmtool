package cli

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// listMgr is one fake host: List answers vms (or err, or blocks), Probe
// answers st. Everything else panics on the nil embedded interface.
type listMgr struct {
	app.Manager
	vms   []vmtool.VMInfo
	err   error
	block bool
	st    app.HostStatus
}

func (l listMgr) List() ([]vmtool.VMInfo, error) {
	if l.block {
		time.Sleep(time.Hour)
	}
	return l.vms, l.err
}
func (l listMgr) Probe(context.Context) app.HostStatus { return l.st }

// installListHosts replaces the resolver: local "jetfire" and the config's
// other hosts are the given fakes; the tokens each remote was built with are
// recorded. No libvirt, no network.
func installListHosts(t *testing.T, mgrs map[string]listMgr) *[]string {
	t.Helper()
	var mu sync.Mutex
	var tokens []string
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		var configured []string
		for n := range e.cfg.Hosts {
			configured = append(configured, n)
		}
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, configured, func() (app.Manager, func(), error) {
			return mgrs[e.cfg.Name], func() {}, nil
		}).WithRemote(func(host, token string) (app.Manager, func(), error) {
			mu.Lock()
			tokens = append(tokens, host+" "+token)
			mu.Unlock()
			return mgrs[host], func() {}, nil
		})
	}
	return &tokens
}

// norm collapses each line's runs of spaces, so the golden text does not
// depend on tabwriter's column widths.
func norm(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n") + "\n"
}

const twoHostsConfig = "name: jetfire\n"

func twoHostCfg(extra string) string {
	return twoHostsConfig + "token_sha256: " + config.Hash(gateToken) + "\nhosts:\n  veritech01: { address: v.labtwo, ssh_user: ubuntu }\n" + extra
}

func TestListAllHosts(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, twoHostCfg("  veritech02: { address: v2.labtwo, ssh_user: ubuntu }\n"), "")
	tokens := installListHosts(t, map[string]listMgr{
		"jetfire":    {vms: []vmtool.VMInfo{{Name: "web", State: vmtool.StateRunning, VCPUs: 2, Memory: 4, IP: "10.0.0.5"}}},
		"veritech01": {vms: []vmtool.VMInfo{{Name: "db", State: vmtool.StateShutoff, VCPUs: 4, Memory: 8, Autostart: true}}},
		"veritech02": {err: errors.New("libvirt:\nconnection refused")},
	})
	r := runGate(t, dir, map[string]bool{"vmtool list": true}, "list")
	if r.err != nil {
		t.Fatalf("list: %v (a failed host must not fail the listing)", r.err)
	}
	want := `NAME STATE VCPUS MEMORY AUTOSTART IP
jetfire/web running 2 4 GiB off 10.0.0.5
veritech01/db shutoff 4 8 GiB on -
veritech02 ERROR: libvirt: connection refused
`
	if got := norm(r.out); got != want {
		t.Errorf("list output:\n%s\nwant:\n%s", got, want)
	}
	if len(*tokens) != 2 {
		t.Errorf("remote clients %q, want veritech01 and veritech02 with the CLI token", *tokens)
	}
	for _, tk := range *tokens {
		if !strings.HasSuffix(tk, " "+gateToken) {
			t.Errorf("remote built with %q", tk)
		}
	}
}

func TestListOneHost(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, twoHostCfg(""), "")
	installListHosts(t, map[string]listMgr{
		"jetfire":    {vms: []vmtool.VMInfo{{Name: "web", State: vmtool.StateRunning}}},
		"veritech01": {vms: []vmtool.VMInfo{{Name: "db", State: vmtool.StateRunning}}},
	})
	keep := map[string]bool{"vmtool list": true}
	r := runGate(t, dir, keep, "list", "veritech01")
	if r.err != nil || strings.Contains(r.out, "jetfire") || !strings.Contains(r.out, "veritech01/db") {
		t.Errorf("list veritech01: %v\n%s", r.err, r.out)
	}
	r = runGate(t, dir, keep, "ls", "jetfire")
	if r.err != nil || strings.Contains(r.out, "veritech01") || !strings.Contains(r.out, "jetfire/web") {
		t.Errorf("ls jetfire: %v\n%s", r.err, r.out)
	}
	r = runGate(t, dir, keep, "list", "nosuch")
	if !errors.Is(r.err, hosts.ErrUnknownHost) {
		t.Errorf("list nosuch: %v, want unknown host", r.err)
	}
}

// Asked for one host that fails, the command fails: there is no table to
// carry the error row for the other hosts.
func TestListOneHostFailure(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, twoHostCfg(""), "")
	installListHosts(t, map[string]listMgr{"veritech01": {err: errors.New("daemon down")}})
	r := runGate(t, dir, map[string]bool{"vmtool list": true}, "list", "veritech01")
	if r.err == nil || !strings.Contains(r.err.Error(), "daemon down") {
		t.Fatalf("err %v", r.err)
	}
}

func TestHostsCommand(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	oldNow := nowFn
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = oldNow })
	dir := writeWorkdir(t, twoHostCfg("  veritech02: { address: v2.labtwo, ssh_user: ubuntu }\n  veritech03: { address: v3.labtwo, ssh_user: ubuntu }\n"), "")
	expiring := now.Add(12*24*time.Hour + time.Hour)
	installListHosts(t, map[string]listMgr{
		"jetfire": {},
		"veritech01": {st: app.HostStatus{Address: "v.labtwo:9473", Reachable: true, APIVersion: "1.0.0",
			CertNotAfter: now.Add(200*24*time.Hour + time.Hour)}},
		"veritech02": {st: app.HostStatus{Address: "v2.labtwo:9473", Reachable: true, APIVersion: "0.9.0",
			CertNotAfter: expiring, Err: errors.New("veritech02 speaks API 0.9.0, this vmtool speaks 1.0.0; run `just deploy veritech02`")}},
		"veritech03": {st: app.HostStatus{Address: "v3.labtwo:9473", Err: errors.New("dial tcp: connection refused")}},
	})
	r := runGate(t, dir, map[string]bool{"vmtool hosts": true}, "hosts")
	if r.err != nil {
		t.Fatal(r.err)
	}
	ours := mustOurVersion(t)
	want := "NAME ADDRESS REACHABLE API CERT_DAYS ERROR\n" +
		"jetfire local yes " + ours + " - -\n" +
		"veritech01 v.labtwo:9473 yes 1.0.0 200 -\n" +
		"veritech02 v2.labtwo:9473 yes 0.9.0 12 veritech02 speaks API 0.9.0, this vmtool speaks 1.0.0; run `just deploy veritech02`\n" +
		"veritech03 v3.labtwo:9473 no - - dial tcp: connection refused\n" +
		"warning: veritech02 certificate expires in 12 days (2026-11-26); run 'just cert veritech02' and 'just deploy veritech02'\n"
	if got := norm(r.out); got != want {
		t.Errorf("hosts output:\n%s\nwant:\n%s", got, want)
	}
}

func TestHostsLocalCertWarning(t *testing.T) {
	// The local host's certificate comes from config.yml tls_cert.
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	now := time.Now()
	oldNow := nowFn
	nowFn = func() time.Time { return now }
	t.Cleanup(func() { nowFn = oldNow })
	dir := writeWorkdir(t, "name: jetfire\ntoken_sha256: "+config.Hash(gateToken)+"\n", "")
	installListHosts(t, map[string]listMgr{})
	old := newResolver
	newResolver = func(e *env) *hosts.Resolver {
		return old(e).WithLocalCert(writeTestCert(t, now.Add(5*24*time.Hour+time.Hour)))
	}
	r := runGate(t, dir, map[string]bool{"vmtool hosts": true}, "hosts")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if !strings.Contains(r.out, "warning: jetfire certificate expires in 5 days (") ||
		!strings.Contains(r.out, "run 'just install'") {
		t.Errorf("no local warning:\n%s", r.out)
	}
}
