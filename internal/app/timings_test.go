package app

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// fakeClock is a clock the fake manager advances: no real time passes.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
}
func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// clockVMs advances the clock by a fixed cost inside each slow call.
type clockVMs struct {
	nwVMs
	clk *fakeClock
}

func (f *clockVMs) CloneImage(b, n, p string, sz uint64) (string, error) {
	f.clk.Advance(300 * time.Millisecond)
	return f.nwVMs.CloneImage(b, n, p, sz)
}
func (f *clockVMs) Start(n string) error {
	f.clk.Advance(1100 * time.Millisecond)
	return f.nwVMs.Start(n)
}
func (f *clockVMs) WaitForIP(n string, d time.Duration) (string, error) {
	f.clk.Advance(2 * time.Second)
	return f.nwVMs.WaitForIP(n, d)
}

func timedCreate(t *testing.T, m *clockVMs) nwRun {
	t.Helper()
	t.Chdir(t.TempDir())
	r := nwRun{log: sshStub(t)}
	r.info, r.err = (&Service{Now: m.clk.Now}).WithTunnel(appTunnel()).CreateVM(context.Background(), m, CreateVMInput{
		Host: "jetfire", Name: "web", Image: "u.qcow2",
		OnProgress: func(ev ProgressEvent) { r.evs = append(r.evs, ev) },
	})
	return r
}

// Every event create emits carries the milliseconds since create began, read
// from the service's clock.
func TestCreateEventsAreStampedWithElapsedMS(t *testing.T) {
	clk := newFakeClock()
	m := &clockVMs{nwVMs: nwVMs{createVMs: createVMs{ip: "10.0.0.5"}}, clk: clk}
	t.Chdir(t.TempDir())
	sshStub(t)
	var evs []ProgressEvent
	_, err := (&Service{Now: clk.Now}).WithTunnel(appTunnel()).CreateVM(context.Background(), m, CreateVMInput{
		Host: "jetfire", Name: "web", Image: "u.qcow2",
		OnProgress: func(ev ProgressEvent) { evs = append(evs, ev) },
	})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		stage, status string
		ms            int64
	}
	var got []row
	for _, ev := range evs {
		got = append(got, row{ev.Stage, ev.Status, ev.ElapsedMS})
	}
	want := []row{
		{"clone", "start", 0}, {"clone", "done", 300},
		{"start", "start", 300}, {"start", "done", 1400},
		{"wait_ip", "start", 1400}, {"wait_ip", "done", 3400},
		{"wait_ssh", "start", 3400}, {"wait_ssh", "done", 3400},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stamps\n got %v\nwant %v", got, want)
	}
}

// A create that fails still stamped every event up to and including the
// failing one.
func TestCreateFailureStillStampsUpToTheFailure(t *testing.T) {
	clk := newFakeClock()
	m := &clockVMs{nwVMs: nwVMs{ipErr: errNoIP}, clk: clk}
	r := timedCreate(t, m)
	if r.err == nil {
		t.Fatal("want the no-address error")
	}
	last := r.evs[len(r.evs)-1]
	if last.Stage != "wait_ip" || last.Status != "error" || last.ElapsedMS != 3400 {
		t.Errorf("last event %+v, want wait_ip error at 3400ms", last)
	}
	for i := 1; i < len(r.evs); i++ {
		if r.evs[i].ElapsedMS < r.evs[i-1].ElapsedMS {
			t.Errorf("stamps go backwards at %d: %+v", i, r.evs)
		}
	}
}

// No existing field of an event changed: with the stamp ignored, the events
// are exactly the stage/status/detail/output they were.
func TestCreateEventFieldsUnchangedByStamps(t *testing.T) {
	clk := newFakeClock()
	m := &clockVMs{nwVMs: nwVMs{createVMs: createVMs{ip: "10.0.0.5"}}, clk: clk}
	r := timedCreate(t, m)
	if r.err != nil {
		t.Fatal(r.err)
	}
	for i := range r.evs {
		r.evs[i].ElapsedMS = 0
	}
	want := []ProgressEvent{
		{Stage: "clone", Status: "start", Detail: "cloning image"},
		{Stage: "clone", Status: "done", Detail: "cloned → web.qcow2"},
		{Stage: "start", Status: "start", Detail: "defining and starting"},
		{Stage: "start", Status: "done", Detail: "started"},
		{Stage: "wait_ip", Status: "start", Detail: "waiting for IP"},
		{Stage: "wait_ip", Status: "done", Detail: "IP 10.0.0.5"},
		{Stage: "wait_ssh", Status: "start", Detail: "waiting for SSH"},
		{Stage: "wait_ssh", Status: "done", Detail: "SSH is up"},
	}
	if !reflect.DeepEqual(r.evs, want) {
		t.Fatalf("events\n got %+v\nwant %+v", r.evs, want)
	}
}
