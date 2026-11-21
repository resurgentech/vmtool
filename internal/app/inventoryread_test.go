package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/storeclient"
)

// N9: Service.Inventory is the one read of the store's inventory. The store is
// an in-process loopback TLS double.

type readStore struct {
	ts   *httptest.Server
	mu   sync.Mutex
	urls []string // RequestURI of each GET /v1/inventory
	down bool
}

func newReadStore(t *testing.T) *readStore {
	t.Helper()
	f := &readStore{}
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
		switch r.URL.Path {
		case "/v1/whoami":
			if f.down {
				out(503, map[string]string{"error": "down"})
				return
			}
			out(200, map[string]string{"name": "jared"})
		case "/v1/inventory":
			f.urls = append(f.urls, r.URL.RequestURI())
			if f.down {
				out(503, map[string]string{"error": "down"})
				return
			}
			t0 := time.Date(2026, 11, 21, 1, 2, 3, 0, time.UTC)
			out(200, map[string]any{
				"hosts": []any{map[string]any{"name": "hv1", "last_report": t0, "daemon_version": "2.6.0", "live": 1, "gone": 0}},
				"vms": []any{map[string]any{"host": "hv1", "name": "vm1", "uuid": "00000000-0000-4000-8000-000000000001",
					"state": "running", "vcpus": 2, "memory": 4, "ip": "10.0.0.5", "autostart": false, "firmware": "efi",
					"disks": []any{}, "comment": "placeholder comment", "first_seen": t0, "last_seen": t0, "has_credentials": true}},
				"machines": []any{map[string]any{"name": "nuc1", "kind": "metal"}},
			})
		default:
			out(404, map[string]string{"error": "not found"})
		}
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *readStore) session(notify func(string)) *StoreSession {
	pool := f.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return OpenStoreSession(f.ts.URL, func() (string, error) { return stTok, nil },
		[]storeclient.Option{storeclient.WithRootCAs(pool)}, notify)
}

func TestInventoryReadsTheStore(t *testing.T) {
	f := newReadStore(t)
	svc := (&Service{}).WithStore(f.session(nil))
	inv, err := svc.Inventory(context.Background(), "hv1", true)
	if err != nil {
		t.Fatal(err)
	}
	if inv == nil || len(inv.Hosts) != 1 || len(inv.Vms) != 1 || len(inv.Machines) != 1 || inv.Vms[0].Name != "vm1" {
		t.Fatalf("inventory %+v", inv)
	}
	if len(f.urls) != 1 || f.urls[0] != "/v1/inventory?gone=true&host=hv1" {
		t.Fatalf("store saw %v", f.urls)
	}
	if _, err := svc.Inventory(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
	if f.urls[1] != "/v1/inventory" {
		t.Fatalf("no filters: %v", f.urls)
	}
}

// No store, a store that does not answer, a missing token: one clear message
// of kind unavailable (REST 503), the same text on every surface, no token in
// it, and no fallback notice besides it.
func TestInventoryNeedsTheStore(t *testing.T) {
	down := newReadStore(t)
	down.down = true
	cases := map[string]struct {
		svc  *Service
		want string
	}{
		"not configured": {&Service{}, "no store is configured"},
		"unreachable":    {(&Service{}).WithStore(down.session(nil)), "is not reachable"},
		"no token": {(&Service{}).WithStore(OpenStoreSession(down.ts.URL,
			func() (string, error) { return "", errors.New("no VMTOOL_SERVER_TOKEN in environment or .env") }, nil, nil)), "VMTOOL_SERVER_TOKEN"},
	}
	for name, c := range cases {
		var notes []string
		if c.svc.Store != nil {
			c.svc.Store.notify = func(s string) { notes = append(notes, s) }
		}
		inv, err := c.svc.Inventory(context.Background(), "", false)
		var ae *Error
		if inv != nil || err == nil || !AsError(err, &ae) || ae.Kind != KindUnavailable {
			t.Errorf("%s: %v, %v", name, inv, err)
			continue
		}
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "inventory") {
			t.Errorf("%s: message %q lacks %q", name, err, c.want)
		}
		if strings.Contains(err.Error(), stTok) || len(notes) != 0 {
			t.Errorf("%s: token in message or extra notice: %q %v", name, err, notes)
		}
	}
	// The store answers whoami but the read fails: unavailable too.
	f := newReadStore(t)
	svc := (&Service{}).WithStore(f.session(nil))
	svc.Store.Mode(context.Background())
	f.mu.Lock()
	f.down = true
	f.mu.Unlock()
	_, err := svc.Inventory(context.Background(), "", false)
	var ae *Error
	if err == nil || !AsError(err, &ae) || ae.Kind != KindUnavailable || strings.Contains(err.Error(), stTok) {
		t.Fatalf("read failure: %v", err)
	}
}
