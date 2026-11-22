package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/storeclient"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// V5: the daemon keeps its links up. At start, for every network of this host
// whose bridge is vmtool's, it reads the VNI from the bridge, asks the store
// for the host's networks and links, only when the registry's network of that
// VNI has the same name and lists this host. A fake clock, a fake store and a
// fake host (no helper, no libvirt) stand in for the world.

type linkHost struct {
	netHost
	mu      sync.Mutex
	active  []string // names ListNetworks answers
	openErr error
	opens   int
}

func newLinkHost() *linkHost {
	h := &linkHost{netHost: *newNetHost("hv1", "192.0.2.11")}
	return h
}

func (h *linkHost) ListNetworks() ([]string, error) { return h.active, nil }

func (h *linkHost) SetNetworkLinks(name, ip, dev string, peers []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.netHost.SetNetworkLinks(name, ip, dev, peers)
}

func (h *linkHost) set(name string, vni uint32, member bool) {
	n := &vmtool.NetworkInfo{Name: name, Bridge: vmtool.BridgeName(vni), VNI: vni, Vmtool: true, Active: true, VMs: []string{}}
	if !member {
		n.Forward, n.Gateway, n.Subnet = "nat", "10.77.0.1", "10.77.0.0/24"
	}
	h.nets[name] = n
	h.active = append(h.active, name)
}

type fakeLinkStore struct {
	mu    sync.Mutex
	nets  *storeclient.HostNetworks
	err   error
	calls int
}

func (f *fakeLinkStore) HostNetworks(_ context.Context, host string) (*storeclient.HostNetworks, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.nets, nil
}

func (f *fakeLinkStore) set(err error, nets *storeclient.HostNetworks) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.nets = err, nets
}

func regNet(name string, vni int, home string, self *storeapiSelf, peers ...string) storeclient.HostNetwork {
	hn := storeclient.HostNetwork{Name: name, Vni: vni, Bridge: vmtool.BridgeName(uint32(vni)), Subnet: "10.77.0.0/24", Domain: name + ".test", Home: home, Peers: []storeclient.NetworkPeer{}}
	if self != nil {
		hn.Member = self
	}
	for i, p := range peers {
		hn.Peers = append(hn.Peers, storeclient.NetworkPeer{Host: fmt.Sprintf("peer%d", i), LanIp: p})
	}
	return hn
}

type storeapiSelf = storeclient.HostNetworkSelf

var selfEth = &storeapiSelf{LanIp: "192.0.2.11", LanDev: "eth-test"}

type keeperEnv struct {
	k      *NetLinkKeeper
	host   *linkHost
	store  *fakeLinkStore
	logs   *syncBuf
	afters chan time.Duration // the delay of each backoff timer asked for
	fire   chan time.Time     // the test fires the timer
	passes chan bool
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newKeeperEnv(t *testing.T, watch func(func(string)) (func(), error)) *keeperEnv {
	t.Helper()
	e := &keeperEnv{host: newLinkHost(), store: &fakeLinkStore{}, logs: &syncBuf{},
		afters: make(chan time.Duration, 16), fire: make(chan time.Time), passes: make(chan bool, 16)}
	e.k = NewNetLinkKeeper(NetLinkKeeperConfig{
		Host:  "hv1",
		Store: e.store,
		Open: func() (Manager, func(), error) {
			e.host.mu.Lock()
			e.host.opens++
			err := e.host.openErr
			e.host.mu.Unlock()
			if err != nil {
				return nil, nil, err
			}
			return e.host, func() {}, nil
		},
		Watch: watch,
		After: func(d time.Duration) <-chan time.Time { e.afters <- d; return e.fire },
		Log:   slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	e.k.passDone = func(retry bool) { e.passes <- retry }
	return e
}

func (e *keeperEnv) waitPass(t *testing.T) bool {
	t.Helper()
	select {
	case r := <-e.passes:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper made no pass")
		return false
	}
}

func (e *keeperEnv) waitAfter(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-e.afters:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper asked for no timer")
		return 0
	}
}

func (e *keeperEnv) links() map[string]string {
	e.host.mu.Lock()
	defer e.host.mu.Unlock()
	out := map[string]string{}
	for k, v := range e.host.links {
		out[k] = v
	}
	return out
}

func TestKeeperLinksAMemberFromTheRegistry(t *testing.T) {
	e := newKeeperEnv(t, nil)
	e.host.set("net1", 10000, true)
	e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: []storeclient.HostNetwork{
		regNet("net1", 10000, "home1", selfEth, "192.0.2.13", "192.0.2.12")}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := e.k.Start(ctx)
	if e.waitPass(t) {
		t.Fatal("the pass asks for a retry")
	}
	if got := e.links()["net1"]; got != "192.0.2.11 eth-test 192.0.2.13,192.0.2.12" {
		t.Errorf("link = %q, want the member row's address and interface and the peers", got)
	}
	cancel()
	<-done
}

// A network with the registry's VNI but another name, or not listing this host,
// is not linked; one line says so, once.
func TestKeeperLinksOnlyWhatTheRegistryMatches(t *testing.T) {
	for name, c := range map[string]struct {
		reg  []storeclient.HostNetwork
		log  string
		none bool
	}{
		"another name":              {reg: []storeclient.HostNetwork{regNet("other", 10000, "home1", selfEth, "192.0.2.13")}, log: `net1`},
		"not a member":              {reg: []storeclient.HostNetwork{regNet("net1", 10000, "home1", nil, "192.0.2.13")}, log: `net1`},
		"unknown VNI":               {reg: nil, log: `net1`},
		"one-host home, no members": {reg: []storeclient.HostNetwork{regNet("net1", 10000, "hv1", nil)}, none: true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newKeeperEnv(t, nil)
			// The home form: a member-form network the registry does not list
			// is removed at start (G2), a home form one never is.
			e.host.set("net1", 10000, false)
			e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: c.reg})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.k.Start(ctx)
			e.waitPass(t)
			if got := e.links(); len(got) != 0 {
				t.Errorf("linked %v from a registry that does not match", got)
			}
			if c.none {
				return
			}
			if n := strings.Count(e.logs.String(), "not linked"); n != 1 || !strings.Contains(e.logs.String(), c.log) {
				t.Errorf("log has %d 'not linked' lines (want 1 naming %q):\n%s", n, c.log, e.logs.String())
			}
			// A second pass says nothing more about the same thing.
			e.k.Relink("")
			e.waitPass(t)
			if n := strings.Count(e.logs.String(), "not linked"); n != 1 {
				t.Errorf("the same refusal was logged %d times", n)
			}
		})
	}
}

// A host with no vmtool network never calls the store.
func TestKeeperWithNoVmtoolNetworkNeverAsksTheStore(t *testing.T) {
	e := newKeeperEnv(t, nil)
	e.host.nets["default"] = &vmtool.NetworkInfo{Name: "default", Bridge: "virbr0", Forward: "nat", Subnet: "192.168.122.0/24", Gateway: "192.168.122.1", Active: true}
	e.host.active = []string{"default"}
	e.store.set(errors.New("must not be asked"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.k.Start(ctx)
	if e.waitPass(t) {
		t.Error("a retry was asked for")
	}
	if e.store.calls != 0 {
		t.Errorf("the store was asked %d times", e.store.calls)
	}
}

// The store is down: no link is made from guesses, one line, a retry on the
// backoff (30 s doubling to 5 min), and the first answer links.
func TestKeeperRetriesOnTheBackoffWhileTheStoreIsDown(t *testing.T) {
	e := newKeeperEnv(t, nil)
	e.host.set("net1", 10000, true)
	e.store.set(errors.New("store down"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.k.Start(ctx)

	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if !e.waitPass(t) {
			t.Fatalf("pass %d: no retry asked for", i)
		}
		if d := e.waitAfter(t); d != w {
			t.Fatalf("backoff %d = %v, want %v", i, d, w)
		}
		if len(e.links()) != 0 {
			t.Fatal("a link was made with the store down")
		}
		if i == len(want)-1 {
			// The store answers before the last timer fires: that pass links
			// and asks for no more retries.
			e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: []storeclient.HostNetwork{regNet("net1", 10000, "home1", selfEth, "192.0.2.13")}})
		}
		e.fire <- time.Time{}
	}
	if e.waitPass(t) {
		t.Error("a retry is still asked for after the store answered")
	}
	if got := e.links()["net1"]; got != "192.0.2.11 eth-test 192.0.2.13" {
		t.Errorf("link = %q", got)
	}
	if n := strings.Count(e.logs.String(), "store down"); n != 1 {
		t.Errorf("the outage was logged %d times, want once:\n%s", n, e.logs.String())
	}
	// A new outage is logged again; a backoff that succeeded starts over at 30 s.
	e.store.set(errors.New("store down"), nil)
	e.k.Relink("")
	if !e.waitPass(t) {
		t.Fatal("no retry after the new outage")
	}
	if d := e.waitAfter(t); d != 30*time.Second {
		t.Errorf("backoff after a success = %v, want 30s", d)
	}
	if n := strings.Count(e.logs.String(), "store down"); n != 2 {
		t.Errorf("two outages were logged %d times", n)
	}
}

// Serving is never blocked: Start returns at once whatever the store does.
func TestKeeperStartReturnsAtOnce(t *testing.T) {
	e := newKeeperEnv(t, nil)
	e.host.set("net1", 10000, true)
	block := make(chan struct{})
	e.store = &fakeLinkStore{}
	e.k.c.Store = blockingStore{block}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	go func() { e.k.Start(ctx); close(started) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked on the store")
	}
	close(block)
}

type blockingStore struct{ release chan struct{} }

func (b blockingStore) HostNetworks(ctx context.Context, _ string) (*storeclient.HostNetworks, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil, errors.New("store down")
}

// A "network started" event links that network again, the same way, and logs
// how to repair the VMs' cards.
func TestKeeperRelinksANetworkWhenLibvirtStartsIt(t *testing.T) {
	var emit func(string)
	e := newKeeperEnv(t, func(fn func(string)) (func(), error) { emit = fn; return func() {}, nil })
	e.host.set("net1", 10000, true)
	e.host.set("net2", 10001, true)
	e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: []storeclient.HostNetwork{
		regNet("net1", 10000, "home1", selfEth, "192.0.2.13"), regNet("net2", 10001, "home1", selfEth, "192.0.2.13")}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.k.Start(ctx)
	e.waitPass(t)
	e.host.mu.Lock()
	e.host.linkLog = nil
	e.host.mu.Unlock()

	emit("net2")
	e.waitPass(t)
	e.host.mu.Lock()
	log := append([]string(nil), e.host.linkLog...)
	e.host.mu.Unlock()
	if len(log) != 1 || !strings.HasPrefix(log[0], "net2 ") {
		t.Errorf("links after the event = %q, want only net2", log)
	}
	if !strings.Contains(e.logs.String(), "systemctl restart libvirtd") || !strings.Contains(e.logs.String(), "net2") {
		t.Errorf("no repair hint for net2:\n%s", e.logs.String())
	}
	// An event for a network that is not vmtool's does nothing.
	emit("default")
	e.waitPass(t)
	e.host.mu.Lock()
	n := len(e.host.linkLog)
	e.host.mu.Unlock()
	if n != 1 {
		t.Errorf("an event for another network caused a link: %d", n)
	}
}

func TestKeeperEventsWhileTheStoreIsDownRetryToo(t *testing.T) {
	var emit func(string)
	e := newKeeperEnv(t, func(fn func(string)) (func(), error) { emit = fn; return func() {}, nil })
	e.host.set("net1", 10000, true)
	e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: []storeclient.HostNetwork{regNet("net1", 10000, "home1", selfEth, "192.0.2.13")}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.k.Start(ctx)
	e.waitPass(t)
	e.store.set(errors.New("store down"), nil)
	emit("net1")
	if !e.waitPass(t) {
		t.Fatal("an event with the store down asked for no retry")
	}
}

func TestKeeperAWatchThatCannotStartIsOneLine(t *testing.T) {
	e := newKeeperEnv(t, func(func(string)) (func(), error) { return nil, errors.New("no event loop") })
	e.host.set("net1", 10000, true)
	e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: []storeclient.HostNetwork{regNet("net1", 10000, "home1", selfEth, "192.0.2.13")}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.k.Start(ctx)
	e.waitPass(t)
	if got := e.links()["net1"]; got == "" {
		t.Error("the start-time link was not made because the watch failed")
	}
	if n := strings.Count(e.logs.String(), "no event loop"); n != 1 {
		t.Errorf("the watch failure was logged %d times:\n%s", n, e.logs.String())
	}
}

func TestKeeperALinkFailureRetriesAndLogsOnce(t *testing.T) {
	e := newKeeperEnv(t, nil)
	e.host.set("net1", 10000, true)
	e.host.linkErr = errors.New("vmtool-net link: bridge does not exist")
	e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: []storeclient.HostNetwork{regNet("net1", 10000, "home1", selfEth, "192.0.2.13")}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.k.Start(ctx)
	for i := 0; i < 2; i++ {
		if !e.waitPass(t) {
			t.Fatalf("pass %d: no retry", i)
		}
		e.waitAfter(t)
		e.fire <- time.Time{}
	}
	if n := strings.Count(e.logs.String(), "bridge does not exist"); n != 1 {
		t.Errorf("the same link failure was logged %d times", n)
	}
}

func TestKeeperStopsWithItsContext(t *testing.T) {
	e := newKeeperEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := e.k.Start(ctx)
	e.waitPass(t)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the keeper did not stop")
	}
}
