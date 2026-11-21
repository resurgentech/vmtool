package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// N8: a workflow on host/vm that fails with "the host does not have it"
// delists the VM in the store (store mode only); `vmtool delete` removes the
// inventory row. The store is an in-process loopback TLS double.

const (
	dlUUID = "00000000-0000-4000-8000-000000000003"
	dlHost = "hv1"
	dlVM   = "vm1"
)

var dlRef = VMRef{Host: dlHost, Name: dlVM}

// invStore answers whoami, delist and the row delete, and records the calls.
type invStore struct {
	ts *httptest.Server

	mu        sync.Mutex
	calls     []string // "METHOD path?query", every inventory call, in order
	whoami    int
	live      bool // POST delist finds a live row
	hadCreds  bool
	keptUntil time.Time
	rowExists bool // DELETE row finds the row
	down      bool // inventory routes answer 503
}

func newInvStore(t *testing.T) *invStore {
	t.Helper()
	f := &invStore{live: true, hadCreds: true, rowExists: true, keptUntil: time.Date(2026, 11, 28, 3, 4, 5, 0, time.UTC)}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		if r.Header.Get("Authorization") != "Bearer "+stTok {
			out(401, map[string]string{"error": "unauthorized"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/v1/whoami" {
			f.whoami++
			out(200, map[string]string{"name": "jared"})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/inventory/") {
			f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
		}
		if f.down {
			out(503, map[string]string{"error": "down"})
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delist"):
			if !f.live {
				out(404, map[string]string{"error": "not found"})
				return
			}
			out(200, map[string]any{"uuid": dlUUID, "had_credentials": f.hadCreds, "credentials_kept_until": f.keptUntil})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/inventory/"):
			if !f.rowExists {
				out(404, map[string]string{"error": "not found"})
				return
			}
			w.WriteHeader(204)
		default:
			out(404, map[string]string{"error": "not found"})
		}
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *invStore) session(t *testing.T, notify func(string)) *StoreSession {
	t.Helper()
	pool := f.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return OpenStoreSession(f.ts.URL, func() (string, error) { return stTok, nil },
		[]storeclient.Option{storeclient.WithRootCAs(pool)}, notify)
}

func (f *invStore) invCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// goneVMs is a host that does not have the VM: every operation on it, and
// Info, answer not-found. infoErr / infoOK change what Info says; opErr is
// what the operation under test answers (default: not-found).
type goneVMs struct {
	Manager
	opErr   error
	infoErr error
	infoOK  bool
}

var errGone = fmt.Errorf("looking up domain %q: %w", dlVM, vmtool.ErrNotFound)

func (g *goneVMs) op() error {
	if g.opErr != nil {
		return g.opErr
	}
	return errGone
}
func (g *goneVMs) Start(string) error               { return g.op() }
func (g *goneVMs) Stop(string) error                { return g.op() }
func (g *goneVMs) Resume(string) error              { return g.op() }
func (g *goneVMs) Destroy(string) error             { return g.op() }
func (g *goneVMs) Reboot(string) error              { return g.op() }
func (g *goneVMs) SetAutostart(string, bool) error  { return g.op() }
func (g *goneVMs) MigrateDisk(string, string) error { return g.op() }
func (g *goneVMs) ResizeDisk(string, uint) error    { return g.op() }
func (g *goneVMs) Delete(string, bool) error        { return g.op() }
func (g *goneVMs) AddDisk(string, uint, string) (*vmtool.DiskDevice, error) {
	return nil, g.op()
}
func (g *goneVMs) Info(string) (*vmtool.VMInfo, error) {
	switch {
	case g.infoErr != nil:
		return nil, g.infoErr
	case g.infoOK:
		return &vmtool.VMInfo{Name: dlVM, UUID: dlUUID, IP: "10.0.0.5"}, nil
	}
	return nil, errGone
}

// workflows is every VM-ref workflow that must delist.
var workflows = map[string]func(s *Service, m Manager) error{
	"start":    func(s *Service, m Manager) error { _, err := s.StartVM(context.Background(), m, dlRef); return err },
	"stop":     func(s *Service, m Manager) error { _, err := s.StopVM(context.Background(), m, dlRef); return err },
	"resume":   func(s *Service, m Manager) error { _, err := s.ResumeVM(context.Background(), m, dlRef); return err },
	"poweroff": func(s *Service, m Manager) error { _, err := s.PoweroffVM(context.Background(), m, dlRef); return err },
	"reboot":   func(s *Service, m Manager) error { _, err := s.RebootVM(context.Background(), m, dlRef); return err },
	"autostart": func(s *Service, m Manager) error {
		_, err := s.SetAutostart(context.Background(), m, dlRef, true)
		return err
	},
	"migrate": func(s *Service, m Manager) error {
		_, err := s.MigrateDisk(context.Background(), m, dlRef, "p1")
		return err
	},
	"add-disk": func(s *Service, m Manager) error {
		_, err := s.AddDisk(context.Background(), m, dlRef, 5, "")
		return err
	},
	"resize": func(s *Service, m Manager) error {
		_, err := s.ResizeDisk(context.Background(), m, dlRef, 20, vmtool.DefaultAuth())
		return err
	},
	"exec": func(s *Service, m Manager) error {
		_, err := s.ExecVM(context.Background(), m, dlRef, []string{"true"}, "")
		return err
	},
	"playbook": func(s *Service, m Manager) error {
		_, err := s.RunPlaybook(context.Background(), m, dlRef, "setup_bot.yml", vmtool.DefaultAuth(), nil)
		return err
	},
	"console":       func(s *Service, m Manager) error { _, err := s.ConsoleVM(context.Background(), m, dlRef); return err },
	"prepare-creds": func(s *Service, m Manager) error { return s.PrepareVMCreds(context.Background(), m, dlRef) },
	"creds-fetch":   func(s *Service, m Manager) error { return s.FetchVMCreds(context.Background(), m, dlRef) },
	"delete":        func(s *Service, m Manager) error { return s.DeleteVM(context.Background(), m, dlRef, false) },
}

func dlService(t *testing.T, f *invStore) (*Service, *notices) {
	t.Helper()
	t.Chdir(t.TempDir())
	n := &notices{}
	return (&Service{}).WithTunnel(appTunnel()).WithStore(f.session(t, n.add)), n
}

func noToken(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if strings.Contains(s, stTok) {
			t.Fatalf("the store token is in %q", s)
		}
	}
}

func TestDelistOnMissingVM(t *testing.T) {
	for name, run := range workflows {
		t.Run(name, func(t *testing.T) {
			f := newInvStore(t)
			svc, notes := dlService(t, f)
			err := run(svc, &goneVMs{})
			ae := asAppErr(t, err)
			if ae.Kind != KindNotFound {
				t.Fatalf("kind %v, want not-found (REST 404): %v", ae.Kind, err)
			}
			for _, want := range []string{"hv1/vm1", "hv1 has no VM named vm1", "removed from the store's inventory", "its login is kept in the store until 2026-11-28"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message %q lacks %q", err, want)
				}
			}
			if !vmtool.IsNotFound(err) && ae.Kind != KindNotFound {
				t.Errorf("not-found lost")
			}
			if got := f.invCalls(); len(got) != 1 || got[0] != "POST /v1/inventory/hv1/vm1/delist" {
				t.Errorf("store calls %q, want one delist", got)
			}
			if l := notes.list; len(l) != 0 {
				t.Errorf("notes %q, want none", l)
			}
			noToken(t, err.Error())
		})
	}
}

// The message says what happened to the login only when there was one.
func TestDelistMessageWithoutLogin(t *testing.T) {
	f := newInvStore(t)
	f.hadCreds = false
	svc, _ := dlService(t, f)
	err := workflows["start"](svc, &goneVMs{})
	if !strings.Contains(err.Error(), "removed from the store's inventory") || strings.Contains(err.Error(), "login") {
		t.Errorf("message %q", err)
	}
}

// Only a not-found from Info leads to Delist: a missing pool on add-disk
// (the VM is there) must not delist, nor must any other failure.
func TestNoDelistWhenTheHostHasTheVM(t *testing.T) {
	poolGone := fmt.Errorf("pool %q: %w", "p9", vmtool.ErrNotFound)
	cases := map[string]*goneVMs{
		"missing pool, VM present": {opErr: poolGone, infoOK: true},
		"info fails otherwise":     {infoErr: errors.New("libvirt connection lost")},
		"op fails otherwise":       {opErr: errors.New("boom"), infoErr: errGone},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			f := newInvStore(t)
			svc, _ := dlService(t, f)
			_, err := svc.AddDisk(context.Background(), m, dlRef, 5, "p9")
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "inventory") {
				t.Errorf("message %q mentions the inventory", err)
			}
			if got := f.invCalls(); len(got) != 0 {
				t.Errorf("store calls %q, want none", got)
			}
		})
	}
	// The error is exactly the one the workflow would give without a store.
	f := newInvStore(t)
	svc, _ := dlService(t, f)
	m := &goneVMs{opErr: poolGone, infoOK: true}
	_, withStore := svc.AddDisk(context.Background(), m, dlRef, 5, "p9")
	_, without := (&Service{}).AddDisk(context.Background(), m, dlRef, 5, "p9")
	if withStore.Error() != without.Error() || asAppErr(t, withStore).Kind != KindNotFound {
		t.Errorf("with store %q, without %q", withStore, without)
	}
}

func TestDelistStoreHadNoLiveRow(t *testing.T) {
	f := newInvStore(t)
	f.live = false
	svc, notes := dlService(t, f)
	err := workflows["start"](svc, &goneVMs{})
	_, want := (&Service{}).StartVM(context.Background(), &goneVMs{}, dlRef)
	if err.Error() != want.Error() || asAppErr(t, err).Kind != KindNotFound {
		t.Errorf("error %q, want unchanged %q", err, want)
	}
	if got := f.invCalls(); len(got) != 1 {
		t.Errorf("store calls %q", got)
	}
	if len(notes.list) != 0 {
		t.Errorf("notes %q", notes.list)
	}
}

func TestDelistStoreDownLeavesTheErrorAndSaysNothing(t *testing.T) {
	f := newInvStore(t)
	svc, notes := dlService(t, f)
	svc.Store.Mode(context.Background()) // decided: store mode
	f.mu.Lock()
	f.down = true
	f.mu.Unlock()
	err := workflows["reboot"](svc, &goneVMs{})
	_, want := (&Service{}).RebootVM(context.Background(), &goneVMs{}, dlRef)
	if err.Error() != want.Error() || asAppErr(t, err).Kind != KindNotFound {
		t.Errorf("error %q, want unchanged %q", err, want)
	}
	if len(notes.list) != 0 {
		t.Errorf("notes %q, want none for a down store", notes.list)
	}
}

// A command that has not decided the mode (lifecycle commands never do) and
// whose store is down when the missing VM is touched: the error is unchanged
// and nothing is printed, not even the local-mode fallback line; the mode
// stays undecided.
func TestDelistUndecidedStoreDownSaysNothing(t *testing.T) {
	f := newInvStore(t)
	svc, notes := dlService(t, f)
	f.ts.Close() // refused
	old := WhoamiTimeout
	WhoamiTimeout = 300 * time.Millisecond
	t.Cleanup(func() { WhoamiTimeout = old })
	err := workflows["start"](svc, &goneVMs{})
	_, want := (&Service{}).StartVM(context.Background(), &goneVMs{}, dlRef)
	if err.Error() != want.Error() || asAppErr(t, err).Kind != KindNotFound {
		t.Errorf("error %q, want unchanged %q", err, want)
	}
	if len(notes.list) != 0 {
		t.Errorf("notes %q, want none for a down store", notes.list)
	}
	if n := svc.Store.Notice(); n != "" {
		t.Errorf("mode decided by the touch: notice %q", n)
	}
}

// A command whose mode was already decided local makes no inventory call.
func TestDelistDecidedLocalMakesNoCall(t *testing.T) {
	f := newInvStore(t)
	svc, _ := dlService(t, f)
	svc.Store.tokenErr = errors.New("no token") // the probe then decides local
	svc.Store.Mode(context.Background())
	_ = workflows["start"](svc, &goneVMs{})
	if got := f.invCalls(); len(got) != 0 {
		t.Errorf("store calls %q, want none in local mode", got)
	}
}

// Local mode makes no store call: no session at all, and a session whose
// store was unreachable at the start (local mode) the same.
func TestDelistLocalModeMakesNoStoreCall(t *testing.T) {
	t.Chdir(t.TempDir())
	err := workflows["stop"](&Service{}, &goneVMs{})
	if asAppErr(t, err).Kind != KindNotFound || strings.Contains(err.Error(), "inventory") {
		t.Errorf("error %q", err)
	}

	f := newInvStore(t)
	dead := f.session(t, func(string) {})
	f.ts.Close() // refused: the session falls back to local
	old := WhoamiTimeout
	WhoamiTimeout = 300 * time.Millisecond
	t.Cleanup(func() { WhoamiTimeout = old })
	svc := (&Service{}).WithStore(dead)
	err = workflows["stop"](svc, &goneVMs{})
	if strings.Contains(err.Error(), "inventory") || len(f.invCalls()) != 0 {
		t.Errorf("error %q calls %q", err, f.invCalls())
	}
}

func TestDeleteRemovesTheInventoryRow(t *testing.T) {
	for _, noclone := range []bool{false, true} {
		t.Run(fmt.Sprintf("noclone=%v", noclone), func(t *testing.T) {
			f := newInvStore(t)
			svc, notes := dlService(t, f)
			m := &goneVMs{opErr: nil, infoOK: true}
			dm := &deleteOKVMs{goneVMs: m}
			if err := svc.DeleteVM(context.Background(), dm, dlRef, noclone); err != nil {
				t.Fatal(err)
			}
			want := "DELETE /v1/inventory/hv1/vm1?uuid=" + dlUUID
			if got := f.invCalls(); len(got) != 1 || got[0] != want {
				t.Errorf("store calls %q, want %q", got, want)
			}
			if len(notes.list) != 0 {
				t.Errorf("notes %q", notes.list)
			}
		})
	}
}

// deleteOKVMs is a present VM whose delete succeeds.
type deleteOKVMs struct{ *goneVMs }

func (d *deleteOKVMs) Delete(string, bool) error { return nil }

func TestDeleteIgnoresAnUnknownRow(t *testing.T) {
	f := newInvStore(t)
	f.rowExists = false
	svc, notes := dlService(t, f)
	if err := svc.DeleteVM(context.Background(), &deleteOKVMs{&goneVMs{infoOK: true}}, dlRef, false); err != nil {
		t.Fatal(err)
	}
	if len(f.invCalls()) != 1 || len(notes.list) != 0 {
		t.Errorf("calls %q notes %q", f.invCalls(), notes.list)
	}
}

func TestDeleteWithStoreDownIsOneNote(t *testing.T) {
	f := newInvStore(t)
	svc, notes := dlService(t, f)
	svc.Store.Mode(context.Background())
	f.mu.Lock()
	f.down = true
	f.mu.Unlock()
	if err := svc.DeleteVM(context.Background(), &deleteOKVMs{&goneVMs{infoOK: true}}, dlRef, false); err != nil {
		t.Fatal(err)
	}
	if len(notes.list) != 1 || !strings.Contains(notes.list[0], "hv1/vm1") || !strings.Contains(notes.list[0], "could not be reached") {
		t.Fatalf("notes %q, want one line naming the VM and the unreachable store", notes.list)
	}
	noToken(t, notes.list[0])
}

func TestDeleteLocalModeNoStoreCall(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := (&Service{}).DeleteVM(context.Background(), &deleteOKVMs{&goneVMs{infoOK: true}}, dlRef, false); err != nil {
		t.Fatal(err)
	}
}

// A daemon older than 2.3.0 reports no UUID: the row cannot be named, so no
// call is made.
func TestDeleteWithoutUUIDMakesNoRowCall(t *testing.T) {
	f := newInvStore(t)
	svc, _ := dlService(t, f)
	m := &noUUIDVMs{&deleteOKVMs{&goneVMs{}}}
	if err := svc.DeleteVM(context.Background(), m, dlRef, false); err != nil {
		t.Fatal(err)
	}
	if got := f.invCalls(); len(got) != 0 {
		t.Errorf("store calls %q", got)
	}
}

type noUUIDVMs struct{ *deleteOKVMs }

func (n *noUUIDVMs) Info(string) (*vmtool.VMInfo, error) { return &vmtool.VMInfo{Name: dlVM}, nil }
