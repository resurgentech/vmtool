package hosts

import (
	"context"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
)

// V6: a hosts: entry with vms: false is a network home. The resolver carries
// the mark: NetworkHome, List and Status show it, and a host that is still
// served like any other (it is a valid daemon host).
func TestResolverCarriesNetworkHome(t *testing.T) {
	cfg := &config.Config{
		Name: "ws", Role: config.RoleWorkstation,
		Hosts: map[string]config.Host{
			"hv1":   {Address: "192.0.2.11", SSHUser: "u"},
			"home1": {Address: "192.0.2.13", SSHUser: "u", VMs: boolPtr(false)},
		},
	}
	r := New(cfg)
	if !r.NetworkHome("home1") {
		t.Error("home1 (vms: false) is not a network home")
	}
	if r.NetworkHome("hv1") || r.NetworkHome("ws") || r.NetworkHome("nosuch") {
		t.Error("a host that holds VMs, the own name or an unknown host is a network home")
	}
	var marked []string
	for _, h := range r.List() {
		if h.NetworkHome {
			marked = append(marked, h.Name)
		}
	}
	if len(marked) != 1 || marked[0] != "home1" {
		t.Errorf("List marks %v, want [home1]", marked)
	}
	if err := r.Check("home1"); err != nil {
		t.Errorf("a network home must still be served: %v", err)
	}
}

func TestStatusShowsNetworkHome(t *testing.T) {
	r := NewWith("ws", config.RoleWorkstation, []string{"hv1", "home1"}, okLocal).
		WithNetworkHomes("home1").
		WithRemote(func(host, token string) (app.Manager, func(), error) {
			return probeMgr{st: app.HostStatus{Address: "192.0.2.1:9473", Reachable: true}}, func() {}, nil
		}).WithToken("tok")
	got := r.Status(context.Background(), time.Second)
	if len(got) != 3 {
		t.Fatalf("status %+v", got)
	}
	for _, s := range got {
		if want := s.Host == "home1"; s.NetworkHome != want {
			t.Errorf("%s NetworkHome = %v, want %v", s.Host, s.NetworkHome, want)
		}
	}
}

func boolPtr(b bool) *bool { return &b }
