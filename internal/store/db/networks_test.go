package db

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// V3: the network registry. Placeholders only: networks net1, net2; hosts
// hv1, hv2, home1; LAN addresses in 192.0.2.0/24; interface eth-test.

func mustNet(t *testing.T, d *DB, n NetworkNew) Network {
	t.Helper()
	got, err := d.CreateNetwork(context.Background(), n)
	if err != nil {
		t.Fatalf("create %+v: %v", n, err)
	}
	return got
}

// Migration 6 adds the two tables to a version-5 database and keeps its rows.
func TestMigration6AddsNetworkTables(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d", "store.db")
	d, err := openMigrations(ctx, path, key(1), migrations[:5])
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PutImage(ctx, Image{Name: "img1", SHA256: hash, Size: 1}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(ctx, path, key(1))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if v, _ := d.version(ctx); v != len(migrations) || len(migrations) < 6 {
		t.Fatalf("version %d want %d (at least 6)", v, len(migrations))
	}
	if _, err := d.GetImage(ctx, "img1"); err != nil {
		t.Fatalf("image lost in migration: %v", err)
	}
	n := mustNet(t, d, NetworkNew{Name: "net1", Home: "home1"})
	if n.VNI != FirstAllocatedVNI {
		t.Fatalf("first VNI = %d", n.VNI)
	}
}

func TestCreateNetworkAllocatesVNIAndSubnet(t *testing.T) {
	d, c := invDB(t)
	a := mustNet(t, d, NetworkNew{Name: "net1", Home: "home1"})
	c.t = c.t.Add(time.Minute)
	b := mustNet(t, d, NetworkNew{Name: "net2", Home: "hv1", Domain: "lab.example"})
	if a.VNI != 10000 || a.Subnet != "10.77.0.0/24" || a.Domain != "net1.test" || a.Home != "home1" || !a.Created.Equal(t0) {
		t.Errorf("first = %+v", a)
	}
	if b.VNI != 10001 || b.Subnet != "10.77.1.0/24" || b.Domain != "lab.example" || !b.Created.Equal(t0.Add(time.Minute)) {
		t.Errorf("second = %+v", b)
	}
	if a.Members == nil || len(a.Members) != 0 {
		t.Errorf("members = %#v, want an empty non-nil slice", a.Members)
	}
	got, err := d.GetNetwork(context.Background(), "net2")
	if err != nil || got.VNI != 10001 || got.Subnet != "10.77.1.0/24" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	list, err := d.ListNetworks(context.Background())
	if err != nil || len(list) != 2 || list[0].Name != "net1" || list[1].Name != "net2" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

// A given VNI or subnet is used if free and refused if not; a name is unique;
// a freed VNI and subnet are handed out again, lowest first.
func TestCreateNetworkGivenAndTaken(t *testing.T) {
	ctx := context.Background()
	d, _ := invDB(t)
	mustNet(t, d, NetworkNew{Name: "net1", Home: "home1", VNI: 1, Subnet: "10.77.1.0/24"})
	cases := []struct {
		name string
		in   NetworkNew
		want error
	}{
		{"name taken", NetworkNew{Name: "net1", Home: "hv1"}, ErrNetworkExists},
		{"vni taken", NetworkNew{Name: "net2", Home: "hv1", VNI: 1}, ErrVNITaken},
		{"subnet taken", NetworkNew{Name: "net2", Home: "hv1", Subnet: "10.77.1.0/24"}, ErrSubnetTaken},
	}
	for _, c := range cases {
		if _, err := d.CreateNetwork(ctx, c.in); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// A refused create leaves nothing behind.
	if list, _ := d.ListNetworks(ctx); len(list) != 1 {
		t.Fatalf("after refusals: %+v", list)
	}
	// The allocator skips the given subnet 10.77.1.0 but takes 10.77.0.0.
	n2 := mustNet(t, d, NetworkNew{Name: "net2", Home: "hv1"})
	n3 := mustNet(t, d, NetworkNew{Name: "net3", Home: "hv1"})
	if n2.VNI != 10000 || n2.Subnet != "10.77.0.0/24" || n3.VNI != 10001 || n3.Subnet != "10.77.2.0/24" {
		t.Errorf("allocation after a given one: %+v %+v", n2, n3)
	}
	// Delete frees both for the next create.
	if err := d.DeleteNetwork(ctx, "net2"); err != nil {
		t.Fatal(err)
	}
	n4 := mustNet(t, d, NetworkNew{Name: "net4", Home: "hv1"})
	if n4.VNI != 10000 || n4.Subnet != "10.77.0.0/24" {
		t.Errorf("after delete: %+v", n4)
	}
}

func TestCreateNetworkNoFreeSubnetOrVNI(t *testing.T) {
	ctx := context.Background()
	d, _ := invDB(t)
	for i := 0; i < 256; i++ {
		mustNet(t, d, NetworkNew{Name: fmt.Sprintf("n%d", i), Home: "home1", VNI: uint32(i + 1)})
	}
	if _, err := d.CreateNetwork(ctx, NetworkNew{Name: "extra", Home: "home1"}); !errors.Is(err, ErrNoFreeSubnet) {
		t.Fatalf("257th network: err = %v, want ErrNoFreeSubnet", err)
	}
	d2, _ := invDB(t)
	mustNet(t, d2, NetworkNew{Name: "top", Home: "home1", VNI: MaxVNI})
	mustNet(t, d2, NetworkNew{Name: "mid", Home: "home1", VNI: MaxVNI - 1})
	// A given VNI above the allocator's range is legal; the allocator itself
	// runs out only when every VNI from 10000 to MaxVNI is taken, which a test
	// cannot fill, so only the bounds of a given VNI are checked here.
	if _, err := d2.CreateNetwork(ctx, NetworkNew{Name: "zero-ok", Home: "home1"}); err != nil {
		t.Fatalf("allocation beside a high given VNI: %v", err)
	}
	if _, err := d2.CreateNetwork(ctx, NetworkNew{Name: "big", Home: "home1", VNI: MaxVNI + 1}); err == nil {
		t.Fatalf("a VNI over %d was accepted", MaxVNI)
	}
}

// The transaction test: many concurrent creates never share a VNI or a subnet.
func TestConcurrentCreatesNeverCollide(t *testing.T) {
	ctx := context.Background()
	d, _ := invDB(t)
	const n = 24
	var wg sync.WaitGroup
	out := make([]Network, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], errs[i] = d.CreateNetwork(ctx, NetworkNew{Name: fmt.Sprintf("net%d", i), Home: "home1"})
		}()
	}
	wg.Wait()
	vnis, subnets := map[uint32]string{}, map[string]string{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("create %d: %v", i, errs[i])
		}
		if o, dup := vnis[out[i].VNI]; dup {
			t.Errorf("VNI %d given to %s and %s", out[i].VNI, o, out[i].Name)
		}
		if o, dup := subnets[out[i].Subnet]; dup {
			t.Errorf("subnet %s given to %s and %s", out[i].Subnet, o, out[i].Name)
		}
		vnis[out[i].VNI], subnets[out[i].Subnet] = out[i].Name, out[i].Name
	}
	if list, _ := d.ListNetworks(ctx); len(list) != n {
		t.Errorf("%d networks stored, want %d", len(list), n)
	}
}

func TestNetworkMembers(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	mustNet(t, d, NetworkNew{Name: "net1", Home: "home1"})
	if _, err := d.PutNetworkMember(ctx, "nope", NetworkMember{Host: "hv1", LANIP: "192.0.2.11", LANDev: "eth-test"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member of a missing network: %v", err)
	}
	m, err := d.PutNetworkMember(ctx, "net1", NetworkMember{Host: "hv1", LANIP: "192.0.2.11", LANDev: "eth-test"})
	if err != nil || !m.Joined.Equal(t0) {
		t.Fatalf("add = %+v, %v", m, err)
	}
	c.t = c.t.Add(time.Hour)
	// Again: the address and interface change, the joined time stays.
	m, err = d.PutNetworkMember(ctx, "net1", NetworkMember{Host: "hv1", LANIP: "192.0.2.12", LANDev: "eth-test2"})
	if err != nil || m.LANIP != "192.0.2.12" || m.LANDev != "eth-test2" || !m.Joined.Equal(t0) {
		t.Fatalf("re-add = %+v, %v", m, err)
	}
	if _, err := d.PutNetworkMember(ctx, "net1", NetworkMember{Host: "hv2", LANIP: "192.0.2.13", LANDev: "eth-test"}); err != nil {
		t.Fatal(err)
	}
	n, _ := d.GetNetwork(ctx, "net1")
	if len(n.Members) != 2 || n.Members[0].Host != "hv1" || n.Members[1].Host != "hv2" {
		t.Fatalf("members = %+v", n.Members)
	}
	// Delete is refused while a member remains, and then allowed.
	if err := d.DeleteNetwork(ctx, "net1"); !errors.Is(err, ErrNetworkHasMembers) {
		t.Fatalf("delete with members = %v", err)
	}
	if _, err := d.GetNetwork(ctx, "net1"); err != nil {
		t.Fatalf("a refused delete removed the network: %v", err)
	}
	if err := d.DeleteNetworkMember(ctx, "net1", "hv1"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteNetworkMember(ctx, "net1", "hv1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove = %v", err)
	}
	if err := d.DeleteNetworkMember(ctx, "net1", "hv2"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteNetwork(ctx, "net1"); err != nil {
		t.Fatalf("delete without members = %v", err)
	}
	if err := d.DeleteNetwork(ctx, "net1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
}

// HostNetworks answers the networks a host is a member of or the home of, and
// no other.
func TestHostNetworks(t *testing.T) {
	ctx := context.Background()
	d, _ := invDB(t)
	mustNet(t, d, NetworkNew{Name: "net1", Home: "home1"})
	mustNet(t, d, NetworkNew{Name: "net2", Home: "home1"})
	mustNet(t, d, NetworkNew{Name: "net3", Home: "hv2"})
	for _, m := range []struct{ net, host, ip string }{
		{"net1", "hv1", "192.0.2.11"}, {"net1", "hv2", "192.0.2.12"}, {"net2", "hv2", "192.0.2.12"},
	} {
		if _, err := d.PutNetworkMember(ctx, m.net, NetworkMember{Host: m.host, LANIP: m.ip, LANDev: "eth-test"}); err != nil {
			t.Fatal(err)
		}
	}
	names := func(host string) []string {
		got, err := d.HostNetworks(ctx, host)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range got {
			out = append(out, n.Name)
		}
		return out
	}
	for host, want := range map[string]string{
		"hv1": "[net1]", "hv2": "[net1 net2 net3]", "home1": "[net1 net2]", "hv9": "[]",
	} {
		if got := fmt.Sprint(names(host)); got != want {
			t.Errorf("networks of %s = %s, want %s", host, got, want)
		}
	}
	got, _ := d.HostNetworks(ctx, "hv1")
	if len(got[0].Members) != 2 {
		t.Errorf("members of net1 = %+v, want both", got[0].Members)
	}
}
