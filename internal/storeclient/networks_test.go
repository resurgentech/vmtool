package storeclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// V3: the registry's client methods. Placeholders only; tok is the token.

func TestNetworkRegistryClient(t *testing.T) {
	now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	net1 := Network{Name: "net1", Vni: 10000, Bridge: "vmt-002710", Subnet: "10.77.0.0/24", Domain: "net1.test",
		Home: "home1", Created: now, Members: []NetworkMember{}}
	var bodies []string
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/networks":
			var in NetworkInput
			if json.Unmarshal(b, &in) != nil || in.Name == "taken" {
				js(w, 409, map[string]string{"error": "network name is taken"})
				return
			}
			js(w, 201, net1)
		case "GET /v1/networks":
			js(w, 200, map[string]any{"networks": []Network{net1}})
		case "GET /v1/networks/net1":
			js(w, 200, net1)
		case "DELETE /v1/networks/net1":
			w.WriteHeader(204)
		case "DELETE /v1/networks/full":
			js(w, 409, map[string]string{"error": "network has members"})
		case "PUT /v1/networks/net1/members/hv1":
			js(w, 200, NetworkMember{Host: "hv1", LanIp: "192.0.2.11", LanDev: "eth-test", Joined: now})
		case "DELETE /v1/networks/net1/members/hv1":
			w.WriteHeader(204)
		case "GET /v1/hosts/hv1/networks":
			js(w, 200, HostNetworks{Host: "hv1", Networks: []HostNetwork{{
				Name: "net1", Vni: 10000, Bridge: "vmt-002710", Subnet: "10.77.0.0/24", Domain: "net1.test", Home: "home1",
				Peers: []NetworkPeer{{Host: "hv2", LanIp: "192.0.2.12"}},
			}}})
		case "GET /v1/hosts/hv2/networks":
			js(w, 403, map[string]string{"error": "forbidden"})
		default:
			js(w, 404, map[string]string{"error": "not found"})
		}
	})
	c := f.client(t, tok)
	ctx := context.Background()
	vni := 10000

	got, err := c.CreateNetwork(ctx, NetworkInput{Name: "net1", Home: "home1", Vni: &vni})
	if err != nil || got.Vni != 10000 || got.Bridge != "vmt-002710" {
		t.Fatalf("create = %+v, %v", got, err)
	}
	if !strings.Contains(bodies[0], `"name":"net1"`) || !strings.Contains(bodies[0], `"vni":10000`) || strings.Contains(bodies[0], tok) {
		t.Errorf("create body = %s", bodies[0])
	}
	if _, err := c.CreateNetwork(ctx, NetworkInput{Name: "taken", Home: "home1"}); !errors.Is(err, ErrRegistryConflict) {
		t.Errorf("create taken = %v, want ErrRegistryConflict", err)
	}
	if l, err := c.Networks(ctx); err != nil || len(l) != 1 || l[0].Name != "net1" {
		t.Fatalf("list = %+v, %v", l, err)
	}
	if n, err := c.GetNetwork(ctx, "net1"); err != nil || n.Name != "net1" {
		t.Fatalf("get = %+v, %v", n, err)
	}
	if _, err := c.GetNetwork(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing = %v, want ErrNotFound", err)
	}
	if err := c.DeleteNetwork(ctx, "net1"); err != nil {
		t.Errorf("delete = %v", err)
	}
	if err := c.DeleteNetwork(ctx, "full"); !errors.Is(err, ErrRegistryConflict) {
		t.Errorf("delete with members = %v, want ErrRegistryConflict", err)
	}
	if m, err := c.PutNetworkMember(ctx, "net1", "hv1", NetworkMemberInput{LanIp: "192.0.2.11", LanDev: "eth-test"}); err != nil || m.LanIp != "192.0.2.11" {
		t.Fatalf("put member = %+v, %v", m, err)
	}
	if _, err := c.PutNetworkMember(ctx, "nope", "hv1", NetworkMemberInput{LanIp: "192.0.2.11", LanDev: "eth-test"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("put member of a missing network = %v", err)
	}
	if err := c.DeleteNetworkMember(ctx, "net1", "hv1"); err != nil {
		t.Errorf("delete member = %v", err)
	}
	if err := c.DeleteNetworkMember(ctx, "net1", "hv9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing member = %v", err)
	}
	hn, err := c.HostNetworks(ctx, "hv1")
	if err != nil || hn.Host != "hv1" || len(hn.Networks) != 1 || hn.Networks[0].Peers[0].LanIp != "192.0.2.12" {
		t.Fatalf("host networks = %+v, %v", hn, err)
	}
	if _, err := c.HostNetworks(ctx, "hv2"); !errors.Is(err, ErrForbidden) {
		t.Errorf("host networks of another host = %v, want ErrForbidden", err)
	}

	// Every request carried the bearer token and only there; no error carries it.
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.auths {
		if a != "Bearer "+tok {
			t.Errorf("%s: Authorization %q", f.reqs[i], a)
		}
		if strings.Contains(f.reqs[i], tok) {
			t.Errorf("token in URL %s", f.reqs[i])
		}
	}
}
