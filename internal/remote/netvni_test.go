package remote

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// V3: the remote client sends the network number to the daemon's createNetwork,
// and sends none when it is not given.
func TestCreateNetworkVNISendsVNI(t *testing.T) {
	var bodies []map[string]any
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/hosts/veritech01/networks" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		bodies = append(bodies, m)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"name":"net1","vmtool":true,"bridge":"vmt-002711","vni":10001,"active":true,"autostart":true,"vms":[]}`))
	})
	m := f.manager(t)
	n, err := m.CreateNetworkVNI("net1", "10.77.1.0/24", "lab.example", 10001)
	if err != nil {
		t.Fatal(err)
	}
	if n.Bridge != "vmt-002711" || n.VNI != 10001 {
		t.Errorf("network = %+v", n)
	}
	if _, err := m.CreateNetwork("net2", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies = %v", bodies)
	}
	if bodies[0]["vni"] != float64(10001) || bodies[0]["subnet"] != "10.77.1.0/24" || bodies[0]["name"] != "net1" {
		t.Errorf("body 1 = %v, want name, subnet and vni 10001", bodies[0])
	}
	if _, has := bodies[1]["vni"]; has {
		t.Errorf("body 2 = %v: CreateNetwork must send no vni", bodies[1])
	}
}
