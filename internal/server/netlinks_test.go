package server

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// V5: the daemon keeps its links up. startNetLinks builds the keeper of a
// hypervisor daemon from config.yml the way startInventory builds the reporter:
// the host token, the store, this host's own libvirt, libvirt's network-started
// events. The helper, libvirt and the store are fakes.

type linkMgr struct {
	app.Manager
	mu    sync.Mutex
	links []string
}

func (f *linkMgr) ListNetworks() ([]string, error) { return []string{"net1", "default"}, nil }
func (f *linkMgr) Network(name string) (*vmtool.NetworkInfo, error) {
	if name == "default" {
		return &vmtool.NetworkInfo{Name: "default", Bridge: "virbr0", Forward: "nat", Gateway: "192.168.122.1"}, nil
	}
	return &vmtool.NetworkInfo{Name: "net1", Bridge: "vmt-002710", VNI: 10000, Vmtool: true, Active: true}, nil
}
func (f *linkMgr) SetNetworkLinks(name, ip, dev string, peers []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.links = append(f.links, name+" "+ip+" "+dev+" "+strings.Join(peers, ","))
	return nil
}
func (f *linkMgr) CreateNetworkMember(string, uint32) (*vmtool.NetworkInfo, error) { return nil, nil }
func (f *linkMgr) RemoveNetworkLink(string) error                                  { return nil }
func (f *linkMgr) Underlay(string) (*vmtool.Underlay, error)                       { return nil, nil }
func (f *linkMgr) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.links...)
}

func TestStartNetLinksOffReasons(t *testing.T) {
	var l startLog
	log := slog.New(slog.NewTextHandler(&l, nil))
	r := hosts.NewWith("hv1", config.RoleHypervisor, nil, nil)
	if k, done := startNetLinks(context.Background(), &config.Config{Name: "hv1", Role: config.RoleWorkstation}, r, log, nil); k != nil || done != nil {
		t.Error("a workstation started a keeper")
	}
	if k, _ := startNetLinks(context.Background(), nil, r, log, nil); k != nil {
		t.Error("no config started a keeper")
	}
	cfg := &config.Config{Name: "hv1", Role: config.RoleHypervisor, Dir: t.TempDir()}
	if k, _ := startNetLinks(context.Background(), cfg, r, log, nil); k != nil {
		t.Error("a hypervisor with no store started a keeper")
	}
	text := l.text()
	if n := strings.Count(strings.TrimSpace(text), "\n") + 1; n != 1 || !strings.Contains(text, "store_url") {
		t.Errorf("want one line naming store_url, got:\n%s", text)
	}
}

func TestStartNetLinksLinksAtStartAndOnTheEvent(t *testing.T) {
	const tok = "PLACEHOLDER-not-a-secret-3"
	type got struct{ auth, method, path string }
	seen := make(chan got, 8)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- got{r.Header.Get("Authorization"), r.Method, r.URL.Path}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(storeclient.HostNetworks{Host: "hv1", Networks: []storeclient.HostNetwork{{
			Name: "net1", Vni: 10000, Bridge: "vmt-002710", Subnet: "10.77.0.0/24", Domain: "net1.test", Home: "home1",
			Member: &storeclient.HostNetworkSelf{LanIp: "192.0.2.11", LanDev: "eth-test"},
			Peers:  []storeclient.NetworkPeer{{Host: "home1", LanIp: "192.0.2.13"}},
		}}})
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tok"), []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Name: "hv1", Role: config.RoleHypervisor, Dir: dir, StoreURL: srv.URL, StoreTokenFile: "tok"}
	fm := &linkMgr{}
	r := hosts.NewWith("hv1", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return fm, func() {}, nil })
	var l startLog
	var emit func(string)
	watch := func(fn func(string)) (func(), error) { emit = fn; return func() {}, nil }
	ctx, cancel := context.WithCancel(context.Background())
	k, done := startNetLinks(ctx, cfg, r, slog.New(slog.NewTextHandler(&l, nil)), watch, storeclient.WithRootCAs(pool))
	if k == nil {
		t.Fatalf("not started: %s", l.text())
	}
	t.Cleanup(func() { cancel(); <-done })

	wait := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for len(fm.got()) < n {
			if time.Now().After(deadline) {
				t.Fatalf("links = %q, want %d; log:\n%s", fm.got(), n, l.text())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait(1)
	if g := <-seen; g.auth != "Bearer "+tok || g.method != "GET" || g.path != "/v1/hosts/hv1/networks" {
		t.Fatalf("request: %+v", g)
	}
	if g := fm.got()[0]; g != "net1 192.0.2.11 eth-test 192.0.2.13" {
		t.Errorf("link = %q, want only the vmtool network, from the member row and the peers", g)
	}
	// libvirt starts net1 again: it is linked again.
	if emit == nil {
		t.Fatal("the keeper registered no network event")
	}
	emit("net1")
	wait(2)
	if strings.Contains(l.text(), tok) {
		t.Error("token in log")
	}
}
