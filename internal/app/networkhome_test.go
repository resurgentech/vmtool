package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// V6: a network home runs no VMs, so its daemon's reporter sends a valid, empty
// list (not a failure, not a skipped send): the store then knows the host has
// none.
func TestReporterOfAHostWithNoVMsSendsAnEmptyList(t *testing.T) {
	e := &repEnv{store: newRepStore(), src: &repSource{}, clock: newRepClock(), log: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	e.rep = NewInventoryReporter(InventoryReporterConfig{
		Host: "home1", Version: "v-test", Store: e.store, Source: e.src,
		Now: e.clock.Now, After: e.clock.After,
		Log: slog.New(slog.NewTextHandler(lockedBuf{e.logMu, e.log}, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := e.rep.Start(ctx)
	t.Cleanup(func() { cancel(); <-done })
	e.waitSend(t)
	rep := e.store.last()
	if e.store.hosts[0] != "home1" || rep.Vms == nil || len(rep.Vms) != 0 {
		t.Errorf("report %q %+v, want home1's empty list", e.store.hosts, rep)
	}
}

// V6: create on a network home (a host whose hosts: entry says vms: false) is
// refused before the Manager is called, with a plain message that names the
// host and says what it is for; any other host is untouched.
func TestCreateVMRefusesNetworkHome(t *testing.T) {
	t.Chdir(t.TempDir())
	m := &nwVMs{}
	s := &Service{NetworkHome: func(h string) bool { return h == "home1" }}
	_, err := s.CreateVM(context.Background(), m, CreateVMInput{Host: "home1", Name: "web", Image: "u.qcow2"})
	ae := asAppErr(t, err)
	if ae.Kind != KindConflict {
		t.Errorf("kind %v: %v", ae.Kind, err)
	}
	for _, w := range []string{"home1", "network home", "vms: false"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("message %q lacks %q", err, w)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("the Manager was called: %v", m.calls)
	}

	// Another host goes on to create (it gets as far as the Manager).
	m = &nwVMs{}
	_, _ = s.CreateVM(context.Background(), m, CreateVMInput{Host: "hv1", Name: "web", Image: "u.qcow2"})
	if len(m.calls) == 0 {
		t.Error("create on hv1 never reached the Manager")
	}
}
