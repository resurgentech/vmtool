package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/testexec"
)

func TestTimingsLineFromStamps(t *testing.T) {
	evs := []app.ProgressEvent{
		{Stage: "clone", Status: "start", ElapsedMS: 0},
		{Stage: "clone", Status: "done", ElapsedMS: 300},
		{Stage: "start", Status: "start", ElapsedMS: 300},
		{Stage: "start", Status: "done", ElapsedMS: 1400},
		{Stage: "wait_ip", Status: "start", ElapsedMS: 1400},
		{Stage: "wait_ip", Status: "done", ElapsedMS: 3400},
		{Stage: "wait_ssh", Status: "start", ElapsedMS: 3400},
		{Stage: "wait_ssh", Status: "done", ElapsedMS: 10400},
		{Stage: "grow_disk", Status: "start", ElapsedMS: 10400},
		{Stage: "grow_disk", Status: "done", ElapsedMS: 11000},
	}
	want := "timings: clone 0.3s, start 1.1s, wait_ip 2.0s, wait_ssh 7.0s, grow_disk 0.6s, total 11.0s"
	if got := timingsLine(evs); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// A stage that never started (the no-wait note is a lone done event) is not
// listed; a failure's error event closes its stage and the total.
func TestTimingsLineSkipsStagesThatDidNotRunAndCoversFailure(t *testing.T) {
	evs := []app.ProgressEvent{
		{Stage: "clone", Status: "start", ElapsedMS: 0},
		{Stage: "clone", Status: "done", ElapsedMS: 260},
		{Stage: "grow_disk", Status: "done", ElapsedMS: 900},
		{Stage: "wait_ip", Status: "start", ElapsedMS: 900},
		{Stage: "wait_ip", Status: "error", ElapsedMS: 120900},
	}
	want := "timings: clone 0.3s, wait_ip 120.0s, total 120.9s"
	if got := timingsLine(evs); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := timingsLine(nil); got != "" {
		t.Errorf("no events printed %q", got)
	}
}

type cliClock struct{ t time.Time }

func (c *cliClock) Now() time.Time          { return c.t }
func (c *cliClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// timedFake advances the clock inside the slow calls.
type timedFake struct {
	createFake
	clk *cliClock
}

func (f timedFake) CloneImage(b, n, p string, s uint64) (string, error) {
	f.clk.Advance(300 * time.Millisecond)
	return f.createFake.CloneImage(b, n, p, s)
}
func (f timedFake) Start(n string) error {
	f.clk.Advance(1100 * time.Millisecond)
	return f.createFake.Start(n)
}
func (f timedFake) WaitForIP(n string, d time.Duration) (string, error) {
	f.clk.Advance(2 * time.Second)
	return f.createFake.WaitForIP(n, d)
}

func runTimedCreate(t *testing.T, ip string, args ...string) (string, error) {
	t.Helper()
	testexec.Ansible(t)
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	clk := &cliClock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith("jetfire", config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
			return timedFake{createFake: createFake{log: &createLog{}, ip: ip}, clk: clk}, func() {}, nil
		})
	}
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	var out bytes.Buffer
	root, e := rootFor(dir, &out, io.Discard)
	e.tunnelExe = stubTunnelExe
	e.now = clk.Now
	root.SetArgs(append([]string{"create", "jetfire/web", "u.qcow2", "--inventory", ""}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestCreatePrintsTimingsOnSuccess(t *testing.T) {
	out, err := runTimedCreate(t, "10.0.0.5")
	if err != nil {
		t.Fatal(err)
	}
	want := "timings: clone 0.3s, start 1.1s, wait_ip 2.0s, wait_ssh 0.0s, total 3.4s"
	if !strings.HasSuffix(out, want+"\n") {
		t.Errorf("output does not close with the timings line:\n%s", out)
	}
	if strings.Count(out, "timings:") != 1 {
		t.Errorf("want one timings line:\n%s", out)
	}
}

func TestCreatePrintsTimingsOnFailure(t *testing.T) {
	out, err := runTimedCreate(t, "")
	if err == nil {
		t.Fatal("want the no-address error")
	}
	want := "timings: clone 0.3s, start 1.1s, wait_ip 2.0s, total 3.4s"
	if !strings.Contains(out, want+"\n") || strings.Count(out, "timings:") != 1 {
		t.Errorf("output lacks the failure timings line %q:\n%s", want, out)
	}
}
