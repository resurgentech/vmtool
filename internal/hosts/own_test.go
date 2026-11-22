package hosts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
)

// ownFake records the pool a CloneImage was given, to show which default_pool
// wrapper the resolver applied.
type ownFake struct {
	app.Manager
	pool string
}

func (f *ownFake) CloneImage(_, _, pool string, _ uint64) (string, error) {
	f.pool = pool
	return "", nil
}

func (f *ownFake) Probe(context.Context) app.HostStatus {
	return app.HostStatus{Address: "10.0.0.1:9474", Reachable: true, APIVersion: "1.0.0"}
}

// Machine 1: the own name is listed under hosts:. Every operation on it goes
// to its daemon with the entry's default_pool; the local connector is never
// called; List, AllNames and Status show one probed hypervisor row.
func TestOwnNameListedGoesToDaemon(t *testing.T) {
	fake := &ownFake{}
	var hosts []string
	r := NewWith("ws", config.RoleWorkstation, []string{"hv1", "ws"}, failing(t)).
		WithDefaultPools("top-level-pool", map[string]string{"ws": "nvme"}).
		WithRemote(func(host, token string) (app.Manager, func(), error) {
			hosts = append(hosts, host)
			return fake, func() {}, nil
		}).WithToken("PLACEHOLDER-not-a-secret-1")

	if !r.OwnListed() || r.Local() != "ws" {
		t.Fatalf("OwnListed=%v Local=%q", r.OwnListed(), r.Local())
	}
	if err := r.Check("ws"); err != nil {
		t.Fatal(err)
	}
	if !r.IsRemote("ws") {
		t.Error("listed own name is not remote")
	}
	m, done, err := r.Resolve("ws")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if _, err := m.CloneImage("b", "n", "", 0); err != nil || fake.pool != "nvme" {
		t.Fatalf("CloneImage pool %q err %v, want the entry's default_pool", fake.pool, err)
	}
	if len(hosts) != 1 || hosts[0] != "ws" {
		t.Fatalf("remote connector calls %v", hosts)
	}

	if got := strings.Join(r.AllNames(), ","); got != "ws,hv1" {
		t.Errorf("AllNames = %s", got)
	}
	if got := strings.Join(r.Names(), ","); got != "ws,hv1" {
		t.Errorf("Names = %s", got)
	}
	l := r.List()
	if len(l) != 2 || l[0].Name != "ws" || l[0].Role != config.RoleHypervisor || l[1].Name != "hv1" {
		t.Errorf("List = %+v", l)
	}

	st := r.Status(context.Background(), time.Second)
	if len(st) != 2 || st[0].Host != "ws" || st[1].Host != "hv1" {
		t.Fatalf("Status = %+v", st)
	}
	if s := st[0]; s.Local || !s.Reachable || s.Role != config.RoleHypervisor || s.Address != "10.0.0.1:9474" {
		t.Errorf("own row %+v, want a probed hypervisor row", s)
	}
}

// Listed but without a token or a remote connector: refused like any listed host.
func TestOwnNameListedNeedsToken(t *testing.T) {
	r := NewWith("ws", config.RoleWorkstation, []string{"ws"}, failing(t)).
		WithRemote(func(string, string) (app.Manager, func(), error) {
			t.Error("remote built without a token")
			return nil, nil, errors.New("no")
		})
	if _, _, err := r.Resolve("ws"); err == nil || !strings.Contains(err.Error(), "no token") {
		t.Fatalf("Resolve = %v", err)
	}
}

// New: a config that lists its own name yields one entry, served remotely.
func TestNewOwnListed(t *testing.T) {
	cfg := &config.Config{Name: "ws", Role: config.RoleWorkstation, Hosts: map[string]config.Host{
		"ws":  {Address: "ws.labtwo", DefaultPool: "nvme"},
		"hv1": {Address: "10.0.0.9", Port: 9474},
	}}
	r := New(cfg)
	if !r.OwnListed() || len(r.AllNames()) != 2 {
		t.Fatalf("OwnListed=%v AllNames=%v", r.OwnListed(), r.AllNames())
	}
}

// Machine 2: not listed; local libvirt opens only on Resolve of the own name,
// and a Status row for the own name is the local row.
func TestOwnNameNotListedUsesLocalLibvirt(t *testing.T) {
	opened, closed := 0, 0
	local := func() (app.Manager, func(), error) {
		opened++
		return &ownFake{}, func() { closed++ }, nil
	}
	r := NewWith("ws", config.RoleWorkstation, []string{"hv1"}, local).
		WithRemote(func(host, token string) (app.Manager, func(), error) {
			return &ownFake{}, func() {}, nil
		}).WithToken("PLACEHOLDER-not-a-secret-1")
	if r.OwnListed() {
		t.Fatal("OwnListed true")
	}
	if opened != 0 {
		t.Fatal("libvirt opened at construction")
	}
	r.List()
	r.AllNames()
	if _, _, err := r.Resolve("hv1"); err != nil || opened != 0 {
		t.Fatalf("resolving a remote opened libvirt: %v opened=%d", err, opened)
	}
	_, done, err := r.Resolve("ws")
	if err != nil || opened != 1 {
		t.Fatalf("Resolve own: %v opened=%d", err, opened)
	}
	done()

	st := r.Status(context.Background(), time.Second)
	if len(st) != 2 || st[0].Host != "ws" || !st[0].Local || !st[0].Reachable || st[0].Err != nil || st[0].Address != "local" {
		t.Fatalf("Status = %+v", st)
	}
	if opened != 2 || closed != 2 {
		t.Errorf("Status left libvirt open: opened=%d closed=%d", opened, closed)
	}
}

// Machine 3: neither a listed daemon nor local libvirt (a laptop), with two
// listed remote hosts that resolve and probe normally.
func TestOwnNameNeither(t *testing.T) {
	local := func() (app.Manager, func(), error) { return nil, nil, errors.New("connect: no such file") }
	r := NewWith("laptop", config.RoleWorkstation, []string{"hv2", "hv1"}, local).
		WithRemote(func(host, token string) (app.Manager, func(), error) {
			return &ownFake{}, func() {}, nil
		}).WithToken("PLACEHOLDER-not-a-secret-1")

	for _, h := range []string{"hv1", "hv2"} {
		if _, done, err := r.Resolve(h); err != nil {
			t.Fatalf("Resolve(%s): %v", h, err)
		} else {
			done()
		}
	}
	const want = "no daemon listed for laptop and no local libvirt"
	if _, _, err := r.Resolve("laptop"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Resolve(laptop) = %v, want %q", err, want)
	}

	st := r.Status(context.Background(), time.Second)
	if len(st) != 3 || st[0].Host != "laptop" || st[1].Host != "hv1" || st[2].Host != "hv2" {
		t.Fatalf("Status = %+v", st)
	}
	if s := st[0]; s.Reachable || s.Err == nil || !strings.Contains(s.Err.Error(), want) {
		t.Errorf("laptop row %+v", s)
	}
	for _, s := range st[1:] {
		if !s.Reachable || s.Err != nil {
			t.Errorf("%s row %+v", s.Host, s)
		}
	}
}

// okLocal is a local libvirt that opens and closes without error.
func okLocal() (app.Manager, func(), error) { return &ownFake{}, func() {}, nil }
