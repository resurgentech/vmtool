package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
)

// V1: createNetwork, getNetwork and deleteNetwork through the real handler
// chain, from the remote client (the CLI's way to another host).

func TestNetworkRoutesRoundTrip(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	srv, pool, _ := hypervisor(t, fm, testToken)
	m := remoteFor(t, srv, pool, testToken)

	n, err := m.CreateNetwork("net1", "10.77.1.0/24", "lab.example")
	if err != nil {
		t.Fatal(err)
	}
	want := rtNet("net1")
	if !reflect.DeepEqual(n, want) {
		t.Errorf("CreateNetwork = %+v, want %+v", n, want)
	}
	if _, err := m.CreateNetwork("net2", "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := m.Network("net1")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("Network = %+v, %v", got, err)
	}
	if err := m.DeleteNetwork("net1"); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"CreateNetwork net1 10.77.1.0/24 lab.example", "CreateNetwork net2  ", "Network net1", "DeleteNetwork net1"}
	if g := fm.got(); !reflect.DeepEqual(g, wantCalls) {
		t.Errorf("calls %q, want %q", g, wantCalls)
	}
}

func TestNetworkRouteStatuses(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	srv, pool, _ := hypervisor(t, fm, testToken)
	m := remoteFor(t, srv, pool, testToken)

	kind := func(err error) app.Kind {
		t.Helper()
		var ae *app.Error
		if !app.AsError(err, &ae) {
			t.Fatalf("err = %v, not an *app.Error", err)
		}
		return ae.Kind
	}
	// 400: a name the rule refuses never reaches the host; an overlap does.
	fm.reset()
	for _, bad := range []string{"default", "a b"} {
		if _, err := m.CreateNetwork(bad, "", ""); err == nil || kind(err) != app.KindInvalid {
			t.Errorf("create %q: %v", bad, err)
		}
	}
	if g := fm.got(); len(g) != 0 {
		t.Errorf("a bad name reached the host: %q", g)
	}
	if _, err := m.CreateNetwork("clash", "192.0.2.0/24", ""); err == nil || kind(err) != app.KindInvalid || !strings.Contains(err.Error(), "overlaps route 192.0.2.0/24") {
		t.Errorf("overlap: %v", err)
	}
	// 409.
	if _, err := m.CreateNetwork("taken", "", ""); err == nil || kind(err) != app.KindConflict {
		t.Errorf("taken: %v", err)
	}
	err := m.DeleteNetwork("default")
	if err == nil || kind(err) != app.KindConflict || !strings.Contains(err.Error(), "not made by vmtool") {
		t.Errorf("delete default: %v", err)
	}
	err = m.DeleteNetwork("busy")
	if err == nil || kind(err) != app.KindConflict || !strings.Contains(err.Error(), "vm1, vm2") {
		t.Errorf("delete busy: %v", err)
	}
	// 404.
	if _, err := m.Network("ghost"); err == nil || kind(err) != app.KindNotFound {
		t.Errorf("show ghost: %v", err)
	}
	if err := m.DeleteNetwork("ghost"); err == nil || kind(err) != app.KindNotFound {
		t.Errorf("delete ghost: %v", err)
	}
}

// The network routes need the bearer like every other.
func TestNetworkRoutesNeedBearer(t *testing.T) {
	t.Chdir(t.TempDir())
	srv, pool, _ := hypervisor(t, newRTMgr(t), testToken)
	m := remoteFor(t, srv, pool, "not-the-token")
	if _, err := m.Network("net1"); err == nil {
		t.Error("a wrong token was accepted")
	}
}

// V3: the daemon's createNetwork passes the network number and the subnet to
// the host's Manager, and never needs a store.
func TestNetworkRouteWithVNI(t *testing.T) {
	t.Chdir(t.TempDir())
	fm := newRTMgr(t)
	srv, pool, _ := hypervisor(t, fm, testToken)
	m := remoteFor(t, srv, pool, testToken)

	n, err := m.CreateNetworkVNI("net1", "10.77.1.0/24", "lab.example", 10001)
	if err != nil {
		t.Fatal(err)
	}
	if n.Bridge != "vmt-002711" || n.VNI != 10001 {
		t.Errorf("network = %+v, want bridge vmt-002711", n)
	}
	if g, want := fm.got(), []string{"CreateNetworkVNI net1 10.77.1.0/24 lab.example 10001"}; !reflect.DeepEqual(g, want) {
		t.Errorf("calls %q, want %q", g, want)
	}
	// A bridge number the host already has is its 409, with the cause.
	fm.reset()
	_, err = m.CreateNetworkVNI("taken", "", "", 10002)
	var ae *app.Error
	if !app.AsError(err, &ae) || ae.Kind != app.KindConflict {
		t.Errorf("taken: %v", err)
	}
	// A number beyond 24 bits is 400 and never reaches the host.
	fm.reset()
	_, err = m.CreateNetworkVNI("net1", "", "", 1<<24)
	if !app.AsError(err, &ae) || ae.Kind != app.KindInvalid || len(fm.got()) != 0 {
		t.Errorf("vni 1<<24: %v, calls %q", err, fm.got())
	}
	// Without a number the host numbers itself.
	fm.reset()
	if _, err := m.CreateNetwork("net2", "", ""); err != nil {
		t.Fatal(err)
	}
	if g, want := fm.got(), []string{"CreateNetwork net2  "}; !reflect.DeepEqual(g, want) {
		t.Errorf("calls %q, want %q", g, want)
	}
}
