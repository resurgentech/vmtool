package vmtool

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// G4: the watch reconnects after libvirtd goes away. Only fakes here: no test
// reaches the binding.

type fakeWatchConn struct {
	mu        sync.Mutex
	keepAlive [2]int
	closeCB   func()
	startedCB func(string)
	closed    bool
	dereg     bool
}

func (f *fakeWatchConn) SetKeepAlive(i int, c uint) error {
	f.keepAlive = [2]int{i, int(c)}
	return nil
}
func (f *fakeWatchConn) OnClose(cb func()) error { f.closeCB = cb; return nil }
func (f *fakeWatchConn) OnNetworkStarted(cb func(string)) (func(), error) {
	f.startedCB = cb
	return func() { f.mu.Lock(); f.dereg = true; f.mu.Unlock() }, nil
}
func (f *fakeWatchConn) Close() { f.mu.Lock(); f.closed = true; f.mu.Unlock() }
func (f *fakeWatchConn) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// fakeConnector hands out connections; the first `fail` attempts after the
// first connection fail.
type fakeConnector struct {
	mu       sync.Mutex
	conns    []*fakeWatchConn
	attempts int
	failFrom int // attempts numbered >= failFrom and < failUntil fail
	failTo   int
	tried    chan struct{}
}

func (c *fakeConnector) connect() (watchConn, error) {
	c.mu.Lock()
	c.attempts++
	n := c.attempts
	c.mu.Unlock()
	defer func() {
		if c.tried != nil {
			c.tried <- struct{}{}
		}
	}()
	if n >= c.failFrom && n < c.failTo {
		return nil, errors.New("libvirtd down")
	}
	f := &fakeWatchConn{}
	c.mu.Lock()
	c.conns = append(c.conns, f)
	c.mu.Unlock()
	return f, nil
}

func (c *fakeConnector) conn(i int) *fakeWatchConn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conns[i]
}

type fakeClock struct {
	waits chan time.Duration
	fire  chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{waits: make(chan time.Duration, 32), fire: make(chan time.Time)}
}
func (c *fakeClock) After(d time.Duration) (<-chan time.Time, func()) {
	c.waits <- d
	return c.fire, func() {}
}

func recv[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestNetWatchReconnectsAfterClose(t *testing.T) {
	fc := &fakeConnector{failFrom: 2, failTo: 4} // attempts 2 and 3 fail
	clk := newFakeClock()
	calls := make(chan string, 16)
	var logs syncBuf
	w, err := startNetWatch(fc.connect, clk, func(n string) { calls <- n }, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer w.stop()

	first := fc.conn(0)
	if first.keepAlive != [2]int{watchKeepAliveInterval, watchKeepAliveCount} {
		t.Errorf("keepalive = %v", first.keepAlive)
	}
	first.startedCB("net-a")
	if got := recv(t, calls, "started callback"); got != "net-a" {
		t.Errorf("callback = %q", got)
	}
	select {
	case d := <-clk.waits:
		t.Fatalf("a timer was armed while connected: %v", d)
	default:
	}

	first.closeCB() // libvirtd restarted
	if d := recv(t, clk.waits, "first backoff"); d != time.Second {
		t.Errorf("first backoff = %v, want 1s", d)
	}
	clk.fire <- time.Time{} // attempt 3 fails
	if d := recv(t, clk.waits, "second backoff"); d != 2*time.Second {
		t.Errorf("second backoff = %v, want 2s", d)
	}
	clk.fire <- time.Time{} // attempt 4 succeeds
	if got := recv(t, calls, "re-link pass"); got != "" {
		t.Errorf("recovery call = %q, want \"\" (every network)", got)
	}
	if !first.isClosed() || !first.dereg {
		t.Error("the lost connection was not deregistered and closed")
	}
	second := fc.conn(1)
	if second.startedCB == nil || second.closeCB == nil || second.keepAlive[0] == 0 {
		t.Error("the new connection was not set up (keepalive, close and started callbacks)")
	}
	second.startedCB("net-b")
	if got := recv(t, calls, "second started"); got != "net-b" {
		t.Errorf("callback after recovery = %q", got)
	}
	select {
	case extra := <-calls:
		t.Errorf("extra call %q: want exactly one re-link pass", extra)
	default:
	}
	l := logs.String()
	if strings.Count(l, "connection lost") != 1 || strings.Count(l, "connection recovered") != 1 {
		t.Errorf("want one loss and one recovery line, got:\n%s", l)
	}
	w.stop()
	<-w.exit
	if !second.isClosed() {
		t.Error("stop did not close the connection")
	}
}

func TestNetWatchBackoffDoublesToThirtySeconds(t *testing.T) {
	fc := &fakeConnector{failFrom: 2, failTo: 1 << 30}
	clk := newFakeClock()
	w, err := startNetWatch(fc.connect, clk, func(string) {}, slog.New(slog.NewTextHandler(&syncBuf{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	fc.conn(0).closeCB()
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, d := range want {
		if got := recv(t, clk.waits, "backoff"); got != d*time.Second {
			t.Fatalf("wait %d = %v, want %v", i, got, d*time.Second)
		}
		if i < len(want)-1 {
			clk.fire <- time.Time{}
		}
	}
	w.stop()
	<-w.exit
}

func TestNetWatchStopDuringBackoff(t *testing.T) {
	fc := &fakeConnector{failFrom: 2, failTo: 1 << 30}
	clk := newFakeClock()
	w, err := startNetWatch(fc.connect, clk, func(string) { t.Error("callback ran") }, slog.New(slog.NewTextHandler(&syncBuf{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	fc.conn(0).closeCB()
	recv(t, clk.waits, "backoff")
	w.stop()
	recv(t, w.exit, "the watch to end")
	w.stop() // twice is fine
	fc.mu.Lock()
	n := fc.attempts
	fc.mu.Unlock()
	if n != 2 {
		t.Errorf("attempts = %d, want 2 (no attempt after stop)", n)
	}
}

func TestNetWatchStopWhileConnected(t *testing.T) {
	fc := &fakeConnector{}
	w, err := startNetWatch(fc.connect, newFakeClock(), func(string) {}, slog.New(slog.NewTextHandler(&syncBuf{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	w.stop()
	recv(t, w.exit, "the watch to end")
	if c := fc.conn(0); !c.isClosed() || !c.dereg {
		t.Error("stop did not deregister and close")
	}
}

func TestNetWatchFirstConnectFailureIsReturned(t *testing.T) {
	fc := &fakeConnector{failFrom: 1, failTo: 2}
	w, err := startNetWatch(fc.connect, newFakeClock(), func(string) {}, nil)
	if err == nil || w != nil {
		t.Fatalf("got %v, %v; want an error", w, err)
	}
}
