package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/storeclient"
)

// G2: a waking member cleans up after itself. At the start pass, a member-form
// vmtool network the store does not list this host for (by VNI and name) is
// unlinked, destroyed and undefined, unless a domain has a card on it. A home
// form network is never removed, and with the store down nothing is.

func startOnce(t *testing.T, e *keeperEnv) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.k.Start(ctx)
	e.waitPass(t)
}

func TestKeeperRemovesAMemberNetworkTheRegistryDoesNotListForThisHost(t *testing.T) {
	for name, reg := range map[string][]storeclient.HostNetwork{
		"no network of that VNI":       nil,
		"another name at that VNI":     {regNet("other", 10000, "home1", selfEth, "192.0.2.13")},
		"host not a member of it":      {regNet("net1", 10000, "home1", nil, "192.0.2.13")},
		"only other networks listed":   {regNet("net2", 10001, "home1", selfEth, "192.0.2.13")},
		"the host's row is gone, many": {regNet("net1", 10000, "home1", nil, "192.0.2.13", "192.0.2.12")},
	} {
		t.Run(name, func(t *testing.T) {
			e := newKeeperEnv(t, nil)
			e.host.set("net1", 10000, true)
			e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: reg})
			startOnce(t, e)
			if got := e.host.unlinked; len(got) != 1 || got[0] != "net1" {
				t.Errorf("unlinked = %v, want net1", got)
			}
			if got := e.host.deleted; len(got) != 1 || got[0] != "net1" {
				t.Errorf("deleted = %v, want net1", got)
			}
			if _, ok := e.host.nets["net1"]; ok {
				t.Error("the stale network is still defined")
			}
			if n := strings.Count(e.logs.String(), "removed"); n != 1 || !strings.Contains(e.logs.String(), "net1") {
				t.Errorf("log has %d 'removed' lines naming net1:\n%s", n, e.logs.String())
			}
		})
	}
}

func TestKeeperKeepsWhatItMustNotRemove(t *testing.T) {
	stale := []storeclient.HostNetwork(nil)
	for name, c := range map[string]struct {
		setup func(e *keeperEnv)
		store error
		reg   []storeclient.HostNetwork
	}{
		"a card on it (a defined VM)": {setup: func(e *keeperEnv) { e.host.nets["net1"].VMs = []string{"web1"} }, reg: stale},
		"the home form": {setup: func(e *keeperEnv) {
			e.host.nets["net1"].Forward, e.host.nets["net1"].Gateway, e.host.nets["net1"].Subnet = "nat", "10.77.0.1", "10.77.0.0/24"
		}, reg: stale},
		"the store listing the host": {setup: func(e *keeperEnv) {}, reg: []storeclient.HostNetwork{regNet("net1", 10000, "home1", selfEth, "192.0.2.13")}},
		"the store is down":          {setup: func(e *keeperEnv) {}, store: errors.New("connection refused")},
		"a bridge that is not vmtool's": {setup: func(e *keeperEnv) {
			e.host.nets["net1"].Bridge, e.host.nets["net1"].Vmtool = "virbr7", false
		}, reg: stale},
	} {
		t.Run(name, func(t *testing.T) {
			e := newKeeperEnv(t, nil)
			e.host.set("net1", 10000, true)
			c.setup(e)
			if c.store != nil {
				e.store.set(c.store, nil)
			} else {
				e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: c.reg})
			}
			startOnce(t, e)
			if len(e.host.deleted) != 0 || len(e.host.unlinked) != 0 {
				t.Errorf("unlinked %v, deleted %v, want nothing", e.host.unlinked, e.host.deleted)
			}
			if _, ok := e.host.nets["net1"]; !ok {
				t.Error("the network is gone")
			}
		})
	}
}

// Only the pass at start cleans: a later pass (an event, a Relink) does not.
func TestKeeperCleansOnlyAtStart(t *testing.T) {
	e := newKeeperEnv(t, nil)
	e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: nil})
	startOnce(t, e) // nothing on the host yet
	e.host.set("net1", 10000, true)
	e.k.Relink("net1")
	e.waitPass(t)
	e.k.Relink("")
	e.waitPass(t)
	if len(e.host.deleted) != 0 {
		t.Errorf("a later pass deleted %v", e.host.deleted)
	}
}

// The store was down at the first pass: the retry that gets an answer is the
// start-time pass, and cleans.
func TestKeeperCleansOnTheFirstPassThatTheStoreAnswers(t *testing.T) {
	e := newKeeperEnv(t, nil)
	e.host.set("net1", 10000, true)
	e.store.set(errors.New("connection refused"), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.k.Start(ctx)
	if !e.waitPass(t) {
		t.Fatal("the pass with the store down asks for no retry")
	}
	e.waitAfter(t)
	if len(e.host.deleted) != 0 {
		t.Fatalf("deleted %v with the store down", e.host.deleted)
	}
	e.store.set(nil, &storeclient.HostNetworks{Host: "hv1", Networks: nil})
	e.fire <- time.Now()
	e.waitPass(t)
	if got := e.host.deleted; len(got) != 1 || got[0] != "net1" {
		t.Errorf("deleted = %v, want net1", got)
	}
}
