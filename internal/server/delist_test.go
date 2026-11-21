package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// N8 on the workstation's REST service: a lifecycle call on a VM its host
// does not have delists it in the store, and the mode is decided only then.

// vmMgr is a host with VM "vm1" (present) or without it (gone).
type vmMgr struct {
	app.Manager
	gone bool
}

func (m *vmMgr) err() error {
	if m.gone {
		return fmt.Errorf("looking up domain %q: %w", "vm1", vmtool.ErrNotFound)
	}
	return nil
}
func (m *vmMgr) Start(string) error { return m.err() }
func (m *vmMgr) Info(n string) (*vmtool.VMInfo, error) {
	if err := m.err(); err != nil {
		return nil, err
	}
	return &vmtool.VMInfo{Name: n, State: vmtool.StateRunning}, nil
}

// delistHandler is a workstation service (store configured) over a "hv1"
// host that is m. storeURL "" is what a hypervisor's config gives.
func delistHandler(t *testing.T, s *srvStore, m app.Manager, withStore bool) http.Handler {
	t.Helper()
	t.Chdir(t.TempDir())
	res := hosts.NewWith("hv1", config.RoleWorkstation, nil, func() (app.Manager, func(), error) { return m, func() {}, nil })
	h := &handlers{hosts: res}
	if withStore {
		pool := s.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
		h.storeURL = s.ts.URL
		h.storeToken = func() (string, error) { return srvStoreTok, nil }
		h.storeOpts = []storeclient.Option{storeclient.WithRootCAs(pool)}
	}
	return handlerFor(h)
}

func TestStartOnAMissingVMDelistsIt(t *testing.T) {
	s := newSrvStore(t)
	h := delistHandler(t, s, &vmMgr{gone: true}, true)
	rec := do(h, "POST", "/v1/hosts/hv1/vms/vm1/start", "")
	if rec.Code != 404 {
		t.Fatalf("status %d %s, want 404", rec.Code, rec.Body)
	}
	for _, want := range []string{"hv1/vm1", "hv1 has no VM named vm1", "removed from the store's inventory", "until 2026-11-28"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body %s lacks %q", rec.Body, want)
		}
	}
	if s.delists.Load() != 1 {
		t.Errorf("delist calls %d, want 1", s.delists.Load())
	}
	if strings.Contains(rec.Body.String(), srvStoreTok) || strings.Contains(fmt.Sprint(rec.Header()), srvStoreTok) {
		t.Error("the store token is in the response")
	}
}

// A success never probes: the mode is decided only when a not-found needs it.
func TestStartSuccessMakesNoStoreRequest(t *testing.T) {
	s := newSrvStore(t)
	h := delistHandler(t, s, &vmMgr{}, true)
	if rec := do(h, "POST", "/v1/hosts/hv1/vms/vm1/start", ""); rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if n := s.reqs.Load(); n != 0 {
		t.Errorf("a successful start sent %d requests to the store", n)
	}
}

// A hypervisor's config leaves storeURL empty (server.go): it makes no store
// call on a not-found, and the wiring really does leave it empty.
func TestHypervisorMakesNoStoreCallOnNotFound(t *testing.T) {
	s := newSrvStore(t)
	h := delistHandler(t, s, &vmMgr{gone: true}, false)
	rec := do(h, "POST", "/v1/hosts/hv1/vms/vm1/start", "")
	if rec.Code != 404 || strings.Contains(rec.Body.String(), "inventory") {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if n := s.reqs.Load(); n != 0 {
		t.Errorf("store requests %d, want 0", n)
	}

	t.Setenv(config.ServerTokenEnv, srvStoreTok)
	cfg := &config.Config{Dir: t.TempDir(), Role: config.RoleHypervisor, TokenSHA256: config.Hash(testToken)}
	cfg.Server.URL = s.ts.URL
	srv := &Server{Config: cfg}
	_ = srv.Handler()
	if srv.h.storeURL != "" {
		t.Errorf("a hypervisor's service has storeURL %q", srv.h.storeURL)
	}
}

// The store is unreachable when the missing VM is touched: the client still
// gets the 404 with the original message (no delist text), and no line about
// the down store ("no extra line is printed for a down store").
func TestStartMissingVMWithStoreDown(t *testing.T) {
	s := newSrvStore(t)
	s.down.Store(true)
	h := delistHandler(t, s, &vmMgr{gone: true}, true)
	rec := do(h, "POST", "/v1/hosts/hv1/vms/vm1/start", "")
	if rec.Code != 404 || strings.Contains(rec.Body.String(), "inventory") {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Values(warnHeader); len(got) != 0 {
		t.Errorf("%s = %q, want none for a down store", warnHeader, got)
	}
}
