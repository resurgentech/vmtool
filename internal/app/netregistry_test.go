package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// V3: in store mode `net create` takes its VNI and subnet from the registry
// and then defines the libvirt network on the host with that bridge; in local
// mode it does not touch the store. The store is an in-process loopback TLS
// double; the host is a fake Manager.

// regStore answers whoami and the registry's create and delete routes, and
// records every request.
type regStore struct {
	ts *httptest.Server

	mu        sync.Mutex
	reqs      []string // "METHOD path"
	posted    []storeapi.NetworkInput
	whoami    string // the caller's name; "" answers 503 (the command is in local mode)
	createErr int    // the create route answers this instead of 201
	deleteErr int    // the delete route answers this instead of 204
}

func newRegStore(t *testing.T) *regStore {
	t.Helper()
	f := &regStore{whoami: "jared"}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+stTok {
			out(401, map[string]string{"error": "unauthorized"})
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/whoami":
			if f.whoami == "" {
				out(503, map[string]string{"error": "down"})
				return
			}
			out(200, map[string]string{"name": f.whoami})
		case "POST /v1/networks":
			var in storeapi.NetworkInput
			_ = json.Unmarshal(b, &in)
			f.posted = append(f.posted, in)
			if f.createErr != 0 {
				out(f.createErr, map[string]string{"error": "network name is taken"})
				return
			}
			n := storeapi.Network{Name: in.Name, Vni: 10000, Bridge: "vmt-002710", Subnet: "10.77.0.0/24",
				Domain: in.Name + ".test", Home: in.Home, Created: time.Now(), Members: []storeapi.NetworkMember{}}
			if in.Subnet != nil {
				n.Subnet = *in.Subnet
			}
			if in.Domain != nil {
				n.Domain = *in.Domain
			}
			out(201, n)
		case "DELETE /v1/networks/net1":
			if f.deleteErr != 0 {
				out(f.deleteErr, map[string]string{"error": "boom"})
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

func (f *regStore) session(notes *[]string) *StoreSession {
	pool := f.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return OpenStoreSession(f.ts.URL, func() (string, error) { return stTok, nil },
		[]storeclient.Option{storeclient.WithRootCAs(pool)}, func(l string) {
			if notes != nil {
				*notes = append(*notes, l)
			}
		})
}

func (f *regStore) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *regStore) posts() []storeapi.NetworkInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]storeapi.NetworkInput(nil), f.posted...)
}

// vniMgr is a host that can define a network on a given bridge number.
type vniMgr struct {
	netMgr
	vniCalls []string
	vniErr   error
	oldHost  bool // a daemon that predates the vni field: it numbers the bridge itself
}

func (f *vniMgr) CreateNetworkVNI(name, subnet, domain string, vni uint32) (*vmtool.NetworkInfo, error) {
	f.vniCalls = append(f.vniCalls, fmt.Sprintf("create %s %s %s %d", name, subnet, domain, vni))
	if f.vniErr != nil {
		return nil, f.vniErr
	}
	if f.oldHost {
		vni = 1
	}
	return &vmtool.NetworkInfo{Name: name, Bridge: vmtool.BridgeName(vni), VNI: vni, Vmtool: true, Subnet: subnet, Domain: domain}, nil
}

func regNoToken(t *testing.T, what string, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if strings.Contains(s, stTok) {
			t.Errorf("%s carries the store token: %q", what, s)
		}
	}
}

func TestCreateNetworkInStoreModeUsesTheRegistry(t *testing.T) {
	f := newRegStore(t)
	var notes []string
	s := &Service{Store: f.session(&notes)}
	m := &vniMgr{}
	got, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Bridge != "vmt-002710" || got.VNI != 10000 || got.Subnet != "10.77.0.0/24" || got.Domain != "net1.test" {
		t.Errorf("network = %+v", got)
	}
	if len(m.vniCalls) != 1 || m.vniCalls[0] != "create net1 10.77.0.0/24 net1.test 10000" || len(m.calls) != 0 {
		t.Errorf("the host was asked %v (local %v), want one create with the registry's VNI and subnet", m.vniCalls, m.calls)
	}
	p := f.posts()
	if len(p) != 1 || p[0].Name != "net1" || p[0].Home != "home1" || p[0].Subnet != nil || p[0].Domain != nil || p[0].Vni != nil {
		t.Errorf("registry create = %+v, want name and home only", p)
	}
	if reqs := f.requests(); len(reqs) != 2 || reqs[1] != "POST /v1/networks" {
		t.Errorf("requests = %v", reqs)
	}
	if len(notes) != 0 {
		t.Errorf("notes = %v", notes)
	}
}

func TestCreateNetworkInStoreModePassesGivenSubnetAndDomain(t *testing.T) {
	f := newRegStore(t)
	s := &Service{Store: f.session(nil)}
	m := &vniMgr{}
	if _, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "10.77.1.0/24", "lab.example"); err != nil {
		t.Fatal(err)
	}
	p := f.posts()
	if len(p) != 1 || p[0].Subnet == nil || *p[0].Subnet != "10.77.1.0/24" || p[0].Domain == nil || *p[0].Domain != "lab.example" {
		t.Errorf("registry create = %+v", p)
	}
	if len(m.vniCalls) != 1 || m.vniCalls[0] != "create net1 10.77.1.0/24 lab.example 10000" {
		t.Errorf("host calls = %v", m.vniCalls)
	}
}

// The registry refuses: nothing is made on the host.
func TestCreateNetworkRegistryRefusalLeavesTheHostAlone(t *testing.T) {
	f := newRegStore(t)
	f.createErr = 409
	s := &Service{Store: f.session(nil)}
	m := &vniMgr{}
	_, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "", "")
	if err == nil || kindOfErr(t, err) != KindConflict {
		t.Fatalf("err = %v, want KindConflict", err)
	}
	if len(m.vniCalls)+len(m.calls) != 0 {
		t.Errorf("the host was asked after a refusal: %v %v", m.vniCalls, m.calls)
	}
	regNoToken(t, "error", err.Error())
	// A name that breaks the rule never reaches the store.
	f2 := newRegStore(t)
	s2 := &Service{Store: f2.session(nil)}
	if _, err := s2.CreateNetworkOnHost(context.Background(), "home1", &vniMgr{}, "default", "", ""); err == nil || kindOfErr(t, err) != KindInvalid {
		t.Errorf("name default: %v", err)
	}
	if len(f2.requests()) != 0 {
		t.Errorf("an invalid name reached the store: %v", f2.requests())
	}
}

// The host refuses after the registry accepted: the registry entry is undone.
func TestCreateNetworkUndoesTheRegistryWhenTheHostFails(t *testing.T) {
	f := newRegStore(t)
	s := &Service{Store: f.session(nil)}
	m := &vniMgr{vniErr: fmt.Errorf("x: %w", vmtool.ErrSubnetOverlap)}
	_, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "", "")
	if err == nil || kindOfErr(t, err) != KindInvalid {
		t.Fatalf("err = %v, want the host's kind (KindInvalid)", err)
	}
	reqs := f.requests()
	if len(reqs) != 3 || reqs[1] != "POST /v1/networks" || reqs[2] != "DELETE /v1/networks/net1" {
		t.Errorf("requests = %v, want the create then its delete", reqs)
	}
	regNoToken(t, "error", err.Error())
}

func TestCreateNetworkUndoFailureSaysWhatIsLeft(t *testing.T) {
	f := newRegStore(t)
	f.deleteErr = 500
	s := &Service{Store: f.session(nil)}
	m := &vniMgr{vniErr: fmt.Errorf("libvirt exploded")}
	_, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "", "")
	if err == nil || kindOfErr(t, err) != KindInternal {
		t.Fatalf("err = %v, want the host's kind (KindInternal)", err)
	}
	if !strings.Contains(err.Error(), "net1") || !strings.Contains(err.Error(), "registry") || !strings.Contains(err.Error(), "libvirt exploded") {
		t.Errorf("error = %q, want the host's cause and the registry entry left behind", err)
	}
	regNoToken(t, "error", err.Error())
}

// With no store, or one that is down, or a host token, nothing is registered.
func TestCreateNetworkWithoutARegistryIsV1(t *testing.T) {
	want := &vmtool.NetworkInfo{Name: "net1", Bridge: "vmt-000001", Vmtool: true}
	// no server: block at all
	m := &vniMgr{netMgr: netMgr{info: want}}
	s := &Service{}
	if got, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "", ""); err != nil || got != want {
		t.Fatalf("no store: %v %v", got, err)
	}
	if len(m.calls) != 1 || len(m.vniCalls) != 0 {
		t.Errorf("no store: host calls %v %v, want one local create", m.calls, m.vniCalls)
	}
	// store down: local mode, one notice from the session
	f := newRegStore(t)
	f.whoami = ""
	var notes []string
	m = &vniMgr{netMgr: netMgr{info: want}}
	s = &Service{Store: f.session(&notes)}
	if _, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 || len(f.posts()) != 0 {
		t.Errorf("store down: host calls %v, posts %v", m.calls, f.posts())
	}
	regNoToken(t, "notes", notes...)
	// a host token (the daemon's own session): never registers
	f = newRegStore(t)
	f.whoami = "host/hv1"
	m = &vniMgr{netMgr: netMgr{info: want}}
	s = &Service{Store: f.session(nil)}
	if _, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 || len(f.posts()) != 0 {
		t.Errorf("host token: host calls %v, posts %v", m.calls, f.posts())
	}
}

// A host whose Manager cannot take a number at all (a fake; every real
// Manager can) is refused before anything is registered or made.
func TestCreateNetworkHostWithoutVNISupportIsRefused(t *testing.T) {
	f := newRegStore(t)
	var notes []string
	s := &Service{Store: f.session(&notes)}
	m := &netMgr{info: &vmtool.NetworkInfo{Name: "net1"}}
	_, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "", "")
	if err == nil || !strings.Contains(err.Error(), "host home1 runs a vmtool that cannot take a network number; redeploy it (just deploy home1)") {
		t.Fatalf("err = %v", err)
	}
	if len(m.calls) != 0 || len(f.posts()) != 0 || len(notes) != 0 {
		t.Errorf("host calls %v, posts %v, notes %v; want none", m.calls, f.posts(), notes)
	}
}

// An older daemon ignores the unknown vni field and numbers the bridge
// itself: the network it made is deleted again, and so is the registry entry.
func TestCreateNetworkOldHostIsUndone(t *testing.T) {
	f := newRegStore(t)
	s := &Service{Store: f.session(nil)}
	m := &vniMgr{oldHost: true}
	_, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "", "")
	if err == nil || !strings.Contains(err.Error(), "host home1 runs a vmtool that cannot take a network number; redeploy it (just deploy home1)") {
		t.Fatalf("err = %v", err)
	}
	if len(m.vniCalls) != 1 || len(m.calls) != 1 || m.calls[0] != "delete net1" {
		t.Errorf("host: create %v, then %v; want one create and one delete of net1", m.vniCalls, m.calls)
	}
	reqs := f.requests()
	if len(reqs) != 3 || reqs[2] != "DELETE /v1/networks/net1" {
		t.Errorf("requests = %v, want the registry entry deleted", reqs)
	}
	regNoToken(t, "error", err.Error())
}

// The old host's stray network cannot be deleted either: the error says what
// is left on the host, and the registry entry is still removed.
func TestCreateNetworkOldHostDeleteFailureSaysWhatIsLeft(t *testing.T) {
	f := newRegStore(t)
	s := &Service{Store: f.session(nil)}
	m := &vniMgr{oldHost: true, netMgr: netMgr{err: fmt.Errorf("busy")}}
	_, err := s.CreateNetworkOnHost(context.Background(), "home1", m, "net1", "", "")
	if err == nil || !strings.Contains(err.Error(), "cannot take a network number") ||
		!strings.Contains(err.Error(), "vmt-000001") || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("err = %v, want the redeploy message and the bridge left on the host", err)
	}
	if reqs := f.requests(); reqs[len(reqs)-1] != "DELETE /v1/networks/net1" {
		t.Errorf("requests = %v", reqs)
	}
}

// A bridge number the host already uses, or an overlapping subnet, is the
// host's refusal: its kind and cause come through and the entry is removed.
func TestCreateNetworkHostRefusalKeepsItsKindAndCause(t *testing.T) {
	for _, c := range []struct {
		err  error
		kind Kind
		want string
	}{
		{fmt.Errorf("a bridge vmt-002710 already exists on this host: %w", vmtool.ErrNetworkExists), KindConflict, "vmt-002710"},
		{fmt.Errorf("subnet 10.77.0.0/24 overlaps route 10.77.0.0/16: %w", vmtool.ErrSubnetOverlap), KindInvalid, "overlaps route"},
	} {
		f := newRegStore(t)
		s := &Service{Store: f.session(nil)}
		_, err := s.CreateNetworkOnHost(context.Background(), "home1", &vniMgr{vniErr: c.err}, "net1", "", "")
		if err == nil || kindOfErr(t, err) != c.kind || !strings.Contains(err.Error(), c.want) {
			t.Errorf("err = %v, want kind %v naming %q", err, c.kind, c.want)
		}
		if reqs := f.requests(); reqs[len(reqs)-1] != "DELETE /v1/networks/net1" {
			t.Errorf("requests = %v, want the entry removed", reqs)
		}
	}
}

// The daemon's route: a VNI goes to a host that can take it; without one the
// host numbers itself; a host that cannot take one is refused.
func TestCreateNetworkWithVNI(t *testing.T) {
	s := &Service{}
	m := &vniMgr{}
	if _, err := s.CreateNetworkWithVNI(context.Background(), m, "net1", "10.77.1.0/24", "d.test", 10001); err != nil {
		t.Fatal(err)
	}
	if len(m.vniCalls) != 1 || m.vniCalls[0] != "create net1 10.77.1.0/24 d.test 10001" {
		t.Errorf("calls = %v", m.vniCalls)
	}
	m = &vniMgr{}
	if _, err := s.CreateNetworkWithVNI(context.Background(), m, "net1", "", "", 0); err != nil || len(m.calls) != 1 || len(m.vniCalls) != 0 {
		t.Errorf("no vni: %v, local %v, vni %v", err, m.calls, m.vniCalls)
	}
	if _, err := s.CreateNetworkWithVNI(context.Background(), &netMgr{}, "net1", "", "", 7); err == nil {
		t.Error("a host that cannot take a number was handed one")
	}
	if _, err := s.CreateNetworkWithVNI(context.Background(), &vniMgr{}, "default", "", "", 7); err == nil || kindOfErr(t, err) != KindInvalid {
		t.Errorf("bad name: %v", err)
	}
}
