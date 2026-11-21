package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// listFake answers List after delay (or never, until block is closed).
type listFake struct {
	Manager
	vms   []vmtool.VMInfo
	err   error
	delay time.Duration
	block chan struct{}
}

func (f *listFake) List() ([]vmtool.VMInfo, error) {
	if f.block != nil {
		<-f.block
	}
	time.Sleep(f.delay)
	return f.vms, f.err
}

func TestListAllFanOut(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	fakes := map[string]*listFake{
		"jetfire": {vms: []vmtool.VMInfo{{Name: "a"}}, delay: 100 * time.Millisecond},
		"hv1":     {block: block}, // never answers in time
		"hv2":     {err: errors.New("libvirt exploded")},
		"hv3":     {vms: []vmtool.VMInfo{{Name: "b"}, {Name: "c"}}, delay: 100 * time.Millisecond},
	}
	open := func(host string) (Manager, func(), error) {
		if host == "hv4" {
			return nil, nil, errors.New("no token")
		}
		return fakes[host], func() {}, nil
	}
	start := time.Now()
	got := ListAll(context.Background(), []string{"jetfire", "hv1", "hv2", "hv3", "hv4"}, open, nil, 300*time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > 600*time.Millisecond {
		t.Errorf("took %v: want about the timeout, not the sum", elapsed)
	}
	if len(got) != 5 {
		t.Fatalf("got %d rows", len(got))
	}
	for i, h := range []string{"jetfire", "hv1", "hv2", "hv3", "hv4"} {
		if got[i].Host != h {
			t.Errorf("row %d host %q, want %q (input order)", i, got[i].Host, h)
		}
	}
	if got[0].Err != nil || len(got[0].VMs) != 1 || got[3].Err != nil || len(got[3].VMs) != 2 {
		t.Errorf("healthy hosts: %+v %+v", got[0], got[3])
	}
	if got[1].Err == nil || got[1].Err.Error() != "timed out after 300ms" {
		t.Errorf("slow host err = %v", got[1].Err)
	}
	if got[2].Err == nil || got[2].Err.Error() != "libvirt exploded" {
		t.Errorf("failing host err = %v", got[2].Err)
	}
	if got[4].Err == nil || got[4].Err.Error() != "no token" {
		t.Errorf("unopenable host err = %v", got[4].Err)
	}
}

func TestListTimeoutIsFiveSeconds(t *testing.T) {
	if ListTimeout != 5*time.Second {
		t.Fatalf("ListTimeout = %v", ListTimeout)
	}
	if got := timeoutErr(ListTimeout).Error(); got != "timed out after 5s" {
		t.Fatal(got)
	}
}

type probeFake struct {
	Manager
	st    HostStatus
	delay time.Duration
}

func (p *probeFake) Probe(ctx context.Context) HostStatus {
	select {
	case <-time.After(p.delay):
		return p.st
	case <-ctx.Done():
		return HostStatus{Err: ctx.Err()}
	}
}

func TestProbeAll(t *testing.T) {
	open := func(host string) (Manager, func(), error) {
		switch host {
		case "ok":
			return &probeFake{st: HostStatus{Reachable: true, APIVersion: "1.0.0"}}, func() {}, nil
		case "slow":
			return &probeFake{delay: time.Hour}, func() {}, nil
		case "plain":
			return &listFake{}, func() {}, nil
		}
		return nil, nil, errors.New("no token")
	}
	got := ProbeAll(context.Background(), []string{"ok", "slow", "plain", "bad"}, open, 200*time.Millisecond)
	if len(got) != 4 {
		t.Fatal(got)
	}
	if got[0].Host != "ok" || !got[0].Reachable || got[0].APIVersion != "1.0.0" {
		t.Errorf("ok: %+v", got[0])
	}
	if got[1].Host != "slow" || got[1].Reachable || got[1].Err == nil || !strings.Contains(got[1].Err.Error(), "timed out after 200ms") {
		t.Errorf("slow: %+v", got[1])
	}
	if got[2].Reachable || got[2].Err == nil {
		t.Errorf("plain: %+v", got[2])
	}
	if got[3].Reachable || got[3].Err == nil || got[3].Err.Error() != "no token" {
		t.Errorf("bad: %+v", got[3])
	}
}

func TestCertDaysLeft(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		after time.Time
		days  int
		ok    bool
	}{
		{time.Time{}, 0, false},
		{now.Add(30*24*time.Hour + time.Minute), 30, true},
		{now.Add(29*24*time.Hour + 23*time.Hour), 29, true},
		{now.Add(-time.Hour), -1, true},
	} {
		d, ok := HostStatus{CertNotAfter: tc.after}.CertDaysLeft(now)
		if d != tc.days || ok != tc.ok {
			t.Errorf("%v: %d %v, want %d %v", tc.after, d, ok, tc.days, tc.ok)
		}
	}
}

// A network home is not opened or asked in an all-hosts listing: its row says
// so, with no error and no wait, and the other hosts are listed as before.
func TestListAllSkipsNetworkHome(t *testing.T) {
	var mu sync.Mutex
	var opened []string
	open := func(host string) (Manager, func(), error) {
		mu.Lock()
		opened = append(opened, host)
		mu.Unlock()
		if host == "home1" {
			return nil, nil, errors.New("home1 is unreachable")
		}
		return &listFake{vms: []vmtool.VMInfo{{Name: "a"}}}, func() {}, nil
	}
	home := func(h string) bool { return h == "home1" }
	start := time.Now()
	got := ListAll(context.Background(), []string{"hv1", "home1"}, open, home, 5*time.Second)
	if time.Since(start) > time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	if len(got) != 2 || got[0].Err != nil || len(got[0].VMs) != 1 {
		t.Fatalf("hv1: %+v", got)
	}
	if got[1].Host != "home1" || !got[1].NetworkHome || got[1].Err != nil || len(got[1].VMs) != 0 {
		t.Errorf("home1 row: %+v", got[1])
	}
	for _, h := range opened {
		if h == "home1" {
			t.Error("the network home was opened")
		}
	}
	// nil home: every host is asked.
	got = ListAll(context.Background(), []string{"home1"}, open, nil, time.Second)
	if got[0].NetworkHome || got[0].Err == nil {
		t.Errorf("nil home func: %+v", got[0])
	}
}
