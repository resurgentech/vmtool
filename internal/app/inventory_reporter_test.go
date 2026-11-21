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

	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const (
	repHostTok = "PLACEHOLDER-not-a-secret-3"
	repUUID1   = "00000000-0000-4000-8000-000000000001"
	repUUID2   = "00000000-0000-4000-8000-000000000002"
	repSHA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type repStore struct {
	mu      sync.Mutex
	reports []storeclient.HostReport
	hosts   []string
	errs    []error       // consumed one per call; nil entries or exhausted = success
	gate    chan struct{} // when set, a call blocks until it is closed
	sent    chan int      // the call count, after each call
	entered chan struct{} // one value as each call begins
}

func newRepStore() *repStore {
	return &repStore{sent: make(chan int, 1000), entered: make(chan struct{}, 1000)}
}

func (s *repStore) PutHostInventory(ctx context.Context, host string, in storeclient.HostReport) (*storeclient.ReportResult, error) {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	s.entered <- struct{}{}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	s.reports = append(s.reports, in)
	s.hosts = append(s.hosts, host)
	var err error
	if len(s.errs) > 0 {
		err, s.errs = s.errs[0], s.errs[1:]
	}
	n := len(s.reports)
	s.mu.Unlock()
	s.sent <- n
	if err != nil {
		return nil, err
	}
	return &storeclient.ReportResult{}, nil
}

func (s *repStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reports)
}

func (s *repStore) last() storeclient.HostReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reports[len(s.reports)-1]
}

type repSource struct {
	mu      sync.Mutex
	vms     []vmtool.VMInfo
	listErr error
	diskErr map[string]error
	lists   int
}

func (s *repSource) Open() (InventoryReader, func(), error) { return s, func() {}, nil }

func (s *repSource) List() ([]vmtool.VMInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return append([]vmtool.VMInfo{}, s.vms...), nil
}

func (s *repSource) VMDisks(name string) ([]InventoryDisk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.diskErr[name]; e != nil {
		return nil, e
	}
	return []InventoryDisk{{Target: "vda", Pool: "pool1", Kind: "file", CapacityBytes: 10 << 30}}, nil
}

func (s *repSource) set(vms ...vmtool.VMInfo) {
	s.mu.Lock()
	s.vms = vms
	s.mu.Unlock()
}

func (s *repSource) listCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

type repWait struct {
	d  time.Duration
	ch chan time.Time
}

// repClock is the injected clock: After records the wait; the test fires it.
type repClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan repWait
}

func newRepClock() *repClock {
	return &repClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), waits: make(chan repWait, 100)}
}

func (c *repClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *repClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *repClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.waits <- repWait{d, ch}
	return ch
}

type repEnv struct {
	store *repStore
	src   *repSource
	clock *repClock
	rep   *InventoryReporter
	log   *bytes.Buffer
	logMu *sync.Mutex
}

type lockedBuf struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (l lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (e *repEnv) logText() string {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	return e.log.String()
}

// startReporter starts a reporter over one VM. storeErrs, if any, are the
// store's answers to the first sends, seeded before the reporter starts so the
// start-time send cannot run ahead of them.
func startReporter(t *testing.T, storeErrs ...error) *repEnv {
	t.Helper()
	e := &repEnv{store: newRepStore(), src: &repSource{}, clock: newRepClock(), log: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	e.store.errs = storeErrs
	e.src.set(vmtool.VMInfo{Name: "vm1", State: "running", VCPUs: 2, Memory: 4, IP: "192.0.2.10", Autostart: true,
		UUID: repUUID1, Firmware: "efi", Comment: "placeholder comment",
		Origin: &vmtool.VMOrigin{Image: "img1", ImageSHA256: repSHA, CloneKind: "file", Pool: "pool1",
			Created: time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)}})
	e.rep = NewInventoryReporter(InventoryReporterConfig{
		Host: "hv1", Version: "v-test", Store: e.store, Source: e.src,
		Now: e.clock.Now, After: e.clock.After,
		Log: slog.New(slog.NewTextHandler(lockedBuf{e.logMu, e.log}, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := e.rep.Start(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("reporter did not stop")
		}
	})
	return e
}

func (e *repEnv) waitSend(t *testing.T) int {
	t.Helper()
	select {
	case n := <-e.store.sent:
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no send")
		return 0
	}
}

func (e *repEnv) waitWait(t *testing.T) repWait {
	t.Helper()
	select {
	case w := <-e.clock.waits:
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("no backoff wait requested")
		return repWait{}
	}
}

// The report is the full list, at start, with everything N4 gave and the
// disks; the comment field is always present.
func TestReporterSendsFullListAtStart(t *testing.T) {
	e := startReporter(t)
	e.waitSend(t)
	rep := e.store.last()
	if e.store.hosts[0] != "hv1" || rep.DaemonVersion != "v-test" || !rep.GeneratedAt.Equal(e.clock.Now()) {
		t.Fatalf("envelope: %q %+v", e.store.hosts, rep)
	}
	if len(rep.Vms) != 1 {
		t.Fatalf("vms: %+v", rep.Vms)
	}
	v := rep.Vms[0]
	if v.Name != "vm1" || v.Uuid != repUUID1 || v.State != "running" || v.Vcpus != 2 || v.Memory != 4 ||
		v.Ip != "192.0.2.10" || !v.Autostart || v.Firmware != "efi" {
		t.Errorf("vm: %+v", v)
	}
	if v.Comment == nil || *v.Comment != "placeholder comment" {
		t.Errorf("comment: %v", v.Comment)
	}
	o := v.Origin
	if o == nil || *o.Image != "img1" || *o.ImageSha256 != repSHA || *o.Clone != "file" || *o.Pool != "pool1" || *o.Created != "2026-11-20T01:02:03Z" {
		t.Errorf("origin: %+v", o)
	}
	if len(v.Disks) != 1 || v.Disks[0].Target != "vda" || v.Disks[0].Pool != "pool1" ||
		string(v.Disks[0].Kind) != "file" || v.Disks[0].CapacityBytes != 10<<30 {
		t.Errorf("disks: %+v", v.Disks)
	}
}

// A VM without a comment still carries the field (empty), so clearing reaches
// the store; one without an origin carries none.
func TestReporterAlwaysSendsComment(t *testing.T) {
	env := startReporterWith(t, vmtool.VMInfo{Name: "vm2", State: "shutoff", UUID: repUUID2})
	env.waitSend(t)
	v := env.store.last().Vms[0]
	if v.Comment == nil || *v.Comment != "" {
		t.Errorf("comment: %v", v.Comment)
	}
	if v.Origin != nil {
		t.Errorf("origin: %+v", v.Origin)
	}
}

func startReporterWith(t *testing.T, vms ...vmtool.VMInfo) *repEnv {
	t.Helper()
	// Build like startReporter but with the given VMs before the first send.
	e := &repEnv{store: newRepStore(), src: &repSource{}, clock: newRepClock(), log: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	e.src.set(vms...)
	e.rep = NewInventoryReporter(InventoryReporterConfig{
		Host: "hv1", Version: "v-test", Store: e.store, Source: e.src,
		Now: e.clock.Now, After: e.clock.After,
		Log: slog.New(slog.NewTextHandler(lockedBuf{e.logMu, e.log}, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := e.rep.Start(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return e
}

// Nothing is sent when nothing changed, however much time passes: no timer is
// ever requested, and a later kick is the next send, not a third.
func TestReporterNoTimerDrivenSend(t *testing.T) {
	e := startReporter(t)
	e.waitSend(t)
	e.clock.Advance(30 * 24 * time.Hour)
	if n := e.store.count(); n != 1 {
		t.Fatalf("sends after a month: %d", n)
	}
	select {
	case w := <-e.clock.waits:
		t.Fatalf("a timer was requested: %v", w.d)
	default:
	}
	e.rep.Kick()
	if n := e.waitSend(t); n != 2 {
		t.Fatalf("kick send is #%d, want 2", n)
	}
	if !e.store.last().GeneratedAt.Equal(e.clock.Now()) {
		t.Error("generated_at is not the clock's time")
	}
}

// Kicks while a send runs coalesce into one more send, and Kick never blocks
// even when the store hangs.
func TestReporterKickCoalescesAndNeverBlocks(t *testing.T) {
	e := startReporterGated(t)
	// the start send is blocked on the hung store; hammer Kick
	select {
	case <-e.store.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("start send never began")
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			e.rep.Kick()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Kick blocked behind a hung store")
	}
	close(e.store.gate)
	e.waitSend(t) // the start send
	e.waitSend(t) // the one coalesced send
	e.rep.Kick()
	if n := e.waitSend(t); n != 3 {
		t.Fatalf("third send is #%d: kicks were not coalesced", n)
	}
	if n := e.store.count(); n != 3 {
		t.Fatalf("sends: %d, want 3", n)
	}
}

func startReporterGated(t *testing.T) *repEnv {
	t.Helper()
	e := &repEnv{store: newRepStore(), src: &repSource{}, clock: newRepClock(), log: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	e.store.gate = make(chan struct{})
	e.src.set(vmtool.VMInfo{Name: "vm1", State: "running", UUID: repUUID1})
	e.rep = NewInventoryReporter(InventoryReporterConfig{
		Host: "hv1", Version: "v-test", Store: e.store, Source: e.src,
		Now: e.clock.Now, After: e.clock.After,
		Log: slog.New(slog.NewTextHandler(lockedBuf{e.logMu, e.log}, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := e.rep.Start(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return e
}

// A failing store is retried after 30 s doubling to 5 min, each attempt with a
// freshly built list; a success resets the pace.
func TestReporterBackoff(t *testing.T) {
	boom := errors.New("store unreachable")
	e := startReporter(t, boom, boom, boom, boom, boom, boom)
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	e.waitSend(t)
	for i, d := range want {
		w := e.waitWait(t)
		if w.d != d {
			t.Fatalf("wait %d = %v, want %v", i, w.d, d)
		}
		// the VM list changes while the store is down: the retry sends the new one
		e.src.set(vmtool.VMInfo{Name: fmt.Sprintf("vm%d", i+10), State: "running", UUID: repUUID1})
		w.ch <- time.Time{}
		e.waitSend(t)
		if got := e.store.last().Vms[0].Name; got != fmt.Sprintf("vm%d", i+10) {
			t.Fatalf("retry %d sent a stale list: %s", i, got)
		}
	}
	// the sixth retry succeeded (no more errors): no further wait, pace reset
	e.store.mu.Lock()
	e.store.errs = []error{boom}
	e.store.mu.Unlock()
	e.rep.Kick()
	e.waitSend(t)
	if w := e.waitWait(t); w.d != 30*time.Second {
		t.Fatalf("after a success the wait is %v, want 30s", w.d)
	}
	if strings.Contains(e.logText(), repHostTok) {
		t.Error("token in log")
	}
}

// A kick during a backoff wait does not send at once: the next attempt
// carries it (assumption: the store is failing, so a send now would fail).
func TestReporterKickDuringBackoffWaitsForTheAttempt(t *testing.T) {
	e := startReporter(t, errors.New("down"))
	e.waitSend(t)
	w := e.waitWait(t)
	e.rep.Kick()
	e.rep.Kick()
	if n := e.store.count(); n != 1 {
		t.Fatalf("sent during the wait: %d", n)
	}
	w.ch <- time.Time{}
	e.waitSend(t)
	// the attempt covered the kicks: no extra send follows
	e.rep.Kick()
	if n := e.waitSend(t); n != 3 {
		t.Fatalf("send #%d, want 3 (kicks during the wait folded into the retry)", n)
	}
}

// A listing error sends nothing, then retries on the backoff; an empty list
// is sent when libvirt answered with one.
func TestReporterListingError(t *testing.T) {
	e := &repEnv{store: newRepStore(), src: &repSource{listErr: errors.New("libvirt down")}, clock: newRepClock(), log: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	e.rep = NewInventoryReporter(InventoryReporterConfig{Host: "hv1", Version: "v", Store: e.store, Source: e.src,
		Now: e.clock.Now, After: e.clock.After, Log: slog.New(slog.NewTextHandler(lockedBuf{e.logMu, e.log}, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := e.rep.Start(ctx)
	t.Cleanup(func() { cancel(); <-done })
	w := e.waitWait(t)
	if w.d != 30*time.Second || e.store.count() != 0 {
		t.Fatalf("wait %v, sends %d", w.d, e.store.count())
	}
	e.src.mu.Lock()
	e.src.listErr = nil
	e.src.vms = nil
	e.src.mu.Unlock()
	w.ch <- time.Time{}
	e.waitSend(t)
	if r := e.store.last(); r.Vms == nil || len(r.Vms) != 0 {
		t.Fatalf("an empty list must be sent as one: %+v", r.Vms)
	}
}

// A VM that vanishes between the list and its disks is left out; any other
// disk error sends nothing (a missing VM would be delisted).
func TestReporterDiskErrors(t *testing.T) {
	e := startReporterWith(t,
		vmtool.VMInfo{Name: "vm1", State: "running", UUID: repUUID1},
		vmtool.VMInfo{Name: "vm2", State: "running", UUID: repUUID2})
	e.waitSend(t)
	e.src.mu.Lock()
	e.src.diskErr = map[string]error{"vm2": fmt.Errorf("gone: %w", vmtool.ErrNotFound)}
	e.src.mu.Unlock()
	e.rep.Kick()
	e.waitSend(t)
	if r := e.store.last(); len(r.Vms) != 1 || r.Vms[0].Name != "vm1" {
		t.Fatalf("vanished VM kept: %+v", r.Vms)
	}
	e.src.mu.Lock()
	e.src.diskErr = map[string]error{"vm2": errors.New("libvirt hiccup")}
	e.src.mu.Unlock()
	e.rep.Kick()
	if w := e.waitWait(t); w.d != 30*time.Second {
		t.Fatalf("wait %v", w.d)
	}
	if n := e.store.count(); n != 2 {
		t.Fatalf("a partial list was sent: %d sends", n)
	}
}

// 401 and 403 are logged once per change of state and retried at the slow
// pace; recovery is logged once.
func TestReporterDeniedLoggedOnce(t *testing.T) {
	e := startReporter(t,
		fmt.Errorf("put host inventory: %w", storeclient.ErrUnauthorized),
		fmt.Errorf("put host inventory: %w", storeclient.ErrForbidden),
		fmt.Errorf("put host inventory: %w", storeclient.ErrForbidden),
	)
	e.waitSend(t)
	for i := 0; i < 2; i++ {
		w := e.waitWait(t)
		if w.d != 5*time.Minute {
			t.Fatalf("denied wait %d = %v, want 5m", i, w.d)
		}
		w.ch <- time.Time{}
		e.waitSend(t)
	}
	w := e.waitWait(t)
	w.ch <- time.Time{}
	e.waitSend(t) // succeeds
	e.rep.Kick()
	e.waitSend(t)
	text := e.logText()
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "refused") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("denied lines = %d, want 1:\n%s", n, text)
	}
	if !strings.Contains(text, "reporting resumed") {
		t.Errorf("no recovery line:\n%s", text)
	}
	if strings.Contains(text, repHostTok) {
		t.Error("token in log")
	}
}
