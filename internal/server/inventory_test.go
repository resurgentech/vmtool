package server

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
	"github.com/jaredeh/vmtool/spec"
)

// noVMChange is every operation that leaves what a VM looks like alone. Each
// operation of vmtool.yaml is in this set or in vmChangeOps, so a new route
// cannot be forgotten: the walk below fails on it.
var noVMChange = map[string]string{
	"getDocs": "docs", "getOpenAPISpec": "docs", "getOpenAPIJSON": "docs", "getVersion": "read",
	"listHosts": "read", "listVMs": "read", "getVM": "read",
	"createVM":   "workstation-only workflow; its VM arrives on the daemon as defineVM",
	"resizeDisk": "workstation-only workflow; its change arrives on the daemon as resizeVMVolume",
	"execVM":     "runs a command in the guest", "openVMTunnel": "tunnel", "openVMConsole": "console",
	"runPlaybook": "runs in the guest", "waitVMIP": "read",
	"listNetworks": "read", "getNetwork": "read",
	"createNetwork": "a network is not a VM", "deleteNetwork": "refuses a network with VMs attached",
	"createReservation": "a reservation is in the network; the VM changes with its card (attachInterface)",
	"deleteReservation": "a reservation is in the network; the VM changes with its card (detachInterface)",
	"listBridges":       "read", "listImages": "images, not VMs",
	"deleteImage": "images", "uploadImage": "images", "fetchImage": "images",
	"getImageManifest": "images", "getImageBlob": "images", "getImagePath": "images",
	"listPools": "read", "createPool": "a pool is not a VM",
	"listTemplates": "templates", "createTemplate": "templates", "deleteTemplate": "templates", "exportTemplate": "templates",
	"createVolume": "an unattached volume is not a VM's disk", "cloneVolume": "an unattached volume",
	"getInventory": "read", "setVMComment": "a store write: the comment is in the store's notes, never in the VM",
	"listPlaybooks": "read", "listMachines": "metal machines, not VMs", "addMachine": "machines",
	"getMachine": "machines", "removeMachine": "machines", "rebootMachine": "machines",
	"execMachine": "machines", "openMachineConsole": "machines", "runMachinePlaybook": "machines",
}

func TestEveryOperationIsClassified(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(spec.OpenAPIYAML)
	if err != nil {
		t.Fatal(err)
	}
	var unknown, both []string
	seen := map[string]bool{}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			id := op.OperationID
			seen[id] = true
			_, change := vmChangeOps[id]
			_, none := noVMChange[id]
			switch {
			case change && none:
				both = append(both, id)
			case !change && !none:
				unknown = append(unknown, id)
			}
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("operations in neither vmChangeOps nor noVMChange (decide whether they change a VM): %v", unknown)
	}
	if len(both) > 0 {
		t.Errorf("operations in both sets: %v", both)
	}
	for _, set := range []map[string]bool{vmChangeOps} {
		for id := range set {
			if !seen[id] {
				t.Errorf("vmChangeOps names %q, which is not in the spec", id)
			}
		}
	}
	for id := range noVMChange {
		if !seen[id] {
			t.Errorf("noVMChange names %q, which is not in the spec", id)
		}
	}
	for _, id := range []string{"defineVM", "deleteVMDefinition", "deleteVM", "startVM", "stopVM", "resumeVM", "poweroffVM",
		"rebootVM", "setAutostart", "addDisk", "migrateDisk", "resizeVMVolume", "resizeVolume",
		"attachInterface", "detachInterface"} {
		if !vmChangeOps[id] {
			t.Errorf("%s is missing from the VM change set", id)
		}
	}
	// A comment is a store write: it changes nothing the host reports.
	if vmChangeOps["setVMComment"] || noVMChange["setVMComment"] == "" {
		t.Error("setVMComment must be in noVMChange, not in the VM change set")
	}
}

type kickCount struct {
	mu sync.Mutex
	n  int
}

func (k *kickCount) Kick() { k.mu.Lock(); k.n++; k.mu.Unlock() }
func (k *kickCount) get() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.n
}

func kickServer(t *testing.T, fm *fakeMgr, k Kicker) http.Handler {
	t.Helper()
	t.Chdir(t.TempDir())
	res := hosts.NewWith("box", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return fm, func() {}, nil })
	return handlerFor(&handlers{hosts: res, role: config.RoleHypervisor, kicker: k})
}

// A 2xx answer of a VM-change operation kicks the reporter once; a failure
// and a read do not.
func TestVMChangeKicksOnSuccessOnly(t *testing.T) {
	k := &kickCount{}
	fm := &fakeMgr{}
	h := kickServer(t, fm, k)
	if rec := do(h, "PUT", "/v1/hosts/box/vms/vm1/autostart", `{"enabled":true}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if k.get() != 1 {
		t.Fatalf("kicks after a change: %d", k.get())
	}
	if rec := do(h, "GET", "/v1/hosts/box/vms", ""); rec.Code != 200 {
		t.Fatalf("list: %d", rec.Code)
	}
	if k.get() != 1 {
		t.Fatalf("a read kicked: %d", k.get())
	}
	// refused (bad body): 400, no kick
	if rec := do(h, "PUT", "/v1/hosts/box/vms/vm1/autostart", `{"enabled":"yes"}`); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
	fm.err = fmt.Errorf("looking up domain: %w", vmtool.ErrNotFound)
	if rec := do(h, "PUT", "/v1/hosts/box/vms/ghost/autostart", `{"enabled":true}`); rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
	if k.get() != 1 {
		t.Fatalf("a failed change kicked: %d", k.get())
	}
	// no reporter configured: nothing to kick, nothing breaks
	h2 := kickServer(t, &fakeMgr{}, nil)
	if rec := do(h2, "PUT", "/v1/hosts/box/vms/vm1/autostart", `{"enabled":false}`); rec.Code != 200 {
		t.Fatalf("no reporter: %d", rec.Code)
	}
}

type hangStore struct{ entered chan struct{} }

func (s hangStore) PutHostInventory(ctx context.Context, _ string, _ storeclient.HostReport) (*storeclient.ReportResult, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

type oneVMSource struct{}

func (oneVMSource) Open() (app.InventoryReader, func(), error) { return oneVMSource{}, func() {}, nil }
func (oneVMSource) List() ([]vmtool.VMInfo, error) {
	return []vmtool.VMInfo{{Name: "vm1", UUID: "00000000-0000-4000-8000-000000000001"}}, nil
}
func (oneVMSource) VMDisks(string) ([]app.InventoryDisk, error) { return nil, nil }

// A VM operation's latency and result never depend on the store: the reporter
// is stuck inside a hung store call and the operation still returns at once.
func TestOperationNeverWaitsForTheStore(t *testing.T) {
	st := hangStore{entered: make(chan struct{}, 1)}
	rep := app.NewInventoryReporter(app.InventoryReporterConfig{Host: "box", Version: "v", Store: st, Source: oneVMSource{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := rep.Start(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("reporter did not stop")
		}
	})
	select {
	case <-st.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("reporter never reached the store")
	}
	h := kickServer(t, &fakeMgr{}, rep)
	finished := make(chan int, 1)
	go func() {
		finished <- do(h, "PUT", "/v1/hosts/box/vms/vm1/autostart", `{"enabled":true}`).Code
	}()
	select {
	case code := <-finished:
		if code != 200 {
			t.Fatalf("operation: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the operation waited for the hung store")
	}
}

type startLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *startLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *startLog) text() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// No usable store_token_file or store_url: no reporter, exactly one log line
// naming the reason, never the content.
func TestStartInventoryOffReasons(t *testing.T) {
	const tok = "PLACEHOLDER-not-a-secret-3"
	for name, c := range map[string]struct {
		url, file, content string
		mode               os.FileMode
		want               string
	}{
		"no url":     {"", "tok", tok, 0o600, "store_url"},
		"no key":     {"https://PLACEHOLDER-HOST:8095", "", "", 0, "store_token_file"},
		"missing":    {"https://PLACEHOLDER-HOST:8095", "nofile", "", 0, "does not exist"},
		"loose mode": {"https://PLACEHOLDER-HOST:8095", "tok", tok, 0o644, "0600"},
		"empty":      {"https://PLACEHOLDER-HOST:8095", "tok", "\n", 0o600, "empty"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if c.content != "" {
				if err := os.WriteFile(filepath.Join(dir, "tok"), []byte(c.content), c.mode); err != nil {
					t.Fatal(err)
				}
				_ = os.Chmod(filepath.Join(dir, "tok"), c.mode)
			}
			cfg := &config.Config{Name: "hv1", Role: config.RoleHypervisor, Dir: dir, StoreURL: c.url, StoreTokenFile: c.file}
			var l startLog
			rep, done := startInventory(context.Background(), cfg, hosts.NewWith("hv1", config.RoleHypervisor, nil, nil), slog.New(slog.NewTextHandler(&l, nil)))
			if rep != nil || done != nil {
				t.Fatal("reporter started")
			}
			text := l.text()
			if n := strings.Count(strings.TrimSpace(text), "\n") + 1; n != 1 || !strings.Contains(text, c.want) {
				t.Errorf("want one line naming %q, got:\n%s", c.want, text)
			}
			if strings.Contains(text, tok) {
				t.Error("token in log")
			}
		})
	}
}

// With a store_url, a 0600 token file and the store up, the daemon reports at
// start with the host token as its bearer and its own name in the path.
func TestStartInventoryReportsAtStart(t *testing.T) {
	const tok = "PLACEHOLDER-not-a-secret-3"
	type got struct{ auth, method, path string }
	seen := make(chan got, 4)
	var body []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		body = b.Bytes()
		seen <- got{r.Header.Get("Authorization"), r.Method, r.URL.Path}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"listed": 0, "newly_gone": 0, "relisted": 0, "stale": false})
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tok"), []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Name: "hv1", Role: config.RoleHypervisor, Dir: dir, StoreURL: srv.URL, StoreTokenFile: "tok", IPWaitSeconds: 45}
	fm := &fakeMgr{}
	r := hosts.NewWith("hv1", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return fm, func() {}, nil })
	var l startLog
	ctx, cancel := context.WithCancel(context.Background())
	rep, done := startInventory(ctx, cfg, r, slog.New(slog.NewTextHandler(&l, nil)), storeclient.WithRootCAs(pool))
	if rep == nil {
		t.Fatalf("not started: %s", l.text())
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	if got := rep.IPWait(); got != 45*time.Second {
		t.Errorf("the reporter's address window = %v, want the daemon's ip_wait_seconds (45s)", got)
	}
	select {
	case g := <-seen:
		if g.auth != "Bearer "+tok || g.method != "PUT" || g.path != "/v1/hosts/hv1/inventory" {
			t.Fatalf("request: %+v", g)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no report; log:\n%s", l.text())
	}
	if !json.Valid(body) {
		t.Errorf("body: %s", body)
	}
	if strings.Contains(l.text(), tok) {
		t.Error("token in log")
	}
}
