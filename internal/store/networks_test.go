package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/internal/tokenhash"
)

func testServerNoDB(t *testing.T) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	c := &Config{Listen: "127.0.0.1:8095", Tokens: map[string]string{"alice": tokenhash.Hash(tokA)}}
	ts := httptest.NewServer(NewServer(c, slog.New(slog.NewJSONHandler(&logs, nil)), Deps{}).Handler())
	t.Cleanup(ts.Close)
	return ts, &logs
}

func newJSONReq(method, url, tok, body string) (*http.Request, error) {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// V3: the network registry's routes. Placeholders only: networks net1, net2;
// hosts hv1, hv2, home1; LAN 192.0.2.11-13; interface eth-test. tokA is the
// user token, tokH hv1's host token, tokH2 hv2's.

func (e *invEnv) newNet(t *testing.T, body string) storeapi.Network {
	t.Helper()
	code, raw := e.call(t, "POST", "/v1/networks", tokA, body)
	if code != 201 {
		t.Fatalf("create %s = %d %s", body, code, raw)
	}
	var n storeapi.Network
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *invEnv) member(t *testing.T, network, host, ip string) {
	t.Helper()
	code, raw := e.call(t, "PUT", "/v1/networks/"+network+"/members/"+host, tokA,
		fmt.Sprintf(`{"lan_ip":%q,"lan_dev":"eth-test"}`, ip))
	if code != 200 {
		t.Fatalf("add member %s/%s = %d %s", network, host, code, raw)
	}
}

func (e *invEnv) hostNets(t *testing.T, host, tok string) (int, storeapi.HostNetworks, string) {
	t.Helper()
	code, raw := e.call(t, "GET", "/v1/hosts/"+host+"/networks", tok, "")
	var hn storeapi.HostNetworks
	if code == 200 {
		if err := json.Unmarshal([]byte(raw), &hn); err != nil {
			t.Fatal(err)
		}
	}
	return code, hn, raw
}

func TestNetworkCreateGetListDelete(t *testing.T) {
	e := newInvEnv(t)
	n1 := e.newNet(t, `{"name":"net1","home":"home1"}`)
	if n1.Vni != 10000 || n1.Bridge != "vmt-002710" || n1.Subnet != "10.77.0.0/24" || n1.Domain != "net1.test" ||
		n1.Home != "home1" || len(n1.Members) != 0 || !n1.Created.Equal(e.clock.Now()) {
		t.Fatalf("first network = %+v", n1)
	}
	n2 := e.newNet(t, `{"name":"net2","home":"hv1","domain":"lab.example"}`)
	if n2.Vni != 10001 || n2.Bridge != "vmt-002711" || n2.Subnet != "10.77.1.0/24" || n2.Domain != "lab.example" {
		t.Fatalf("second network = %+v", n2)
	}
	// Given and free: used as given, below 10000 too (a network a host made).
	n3 := e.newNet(t, `{"name":"net3","home":"hv2","vni":1,"subnet":"10.77.9.0/24"}`)
	if n3.Vni != 1 || n3.Bridge != "vmt-000001" || n3.Subnet != "10.77.9.0/24" {
		t.Fatalf("given network = %+v", n3)
	}
	code, raw := e.call(t, "GET", "/v1/networks/net2", tokA, "")
	var got storeapi.Network
	if code != 200 || json.Unmarshal([]byte(raw), &got) != nil || got.Name != "net2" || got.Vni != 10001 {
		t.Fatalf("get = %d %s", code, raw)
	}
	code, raw = e.call(t, "GET", "/v1/networks", tokA, "")
	var list storeapi.NetworkList
	if code != 200 || json.Unmarshal([]byte(raw), &list) != nil || len(list.Networks) != 3 ||
		list.Networks[0].Name != "net1" || list.Networks[2].Name != "net3" {
		t.Fatalf("list = %d %s", code, raw)
	}
	if code, _ := e.call(t, "GET", "/v1/networks/nope", tokA, ""); code != 404 {
		t.Errorf("get missing = %d, want 404", code)
	}
	if code, _ := e.call(t, "DELETE", "/v1/networks/nope", tokA, ""); code != 404 {
		t.Errorf("delete missing = %d, want 404", code)
	}
	if code, _ := e.call(t, "DELETE", "/v1/networks/net1", tokA, ""); code != 204 {
		t.Errorf("delete = %d, want 204", code)
	}
	// Its VNI and subnet are free again.
	if n := e.newNet(t, `{"name":"net4","home":"home1"}`); n.Vni != 10000 || n.Subnet != "10.77.0.0/24" {
		t.Errorf("after delete = %+v", n)
	}
	e.noSecretsInLogs(t)
}

func TestNetworkCreateRefusals(t *testing.T) {
	e := newInvEnv(t)
	e.newNet(t, `{"name":"net1","home":"home1","vni":1,"subnet":"10.77.1.0/24"}`)
	for _, c := range []struct{ name, body, want string }{
		{"name taken", `{"name":"net1","home":"hv1"}`, "network name is taken"},
		{"vni taken", `{"name":"net2","home":"hv1","vni":1}`, "vni is taken"},
		{"subnet taken", `{"name":"net2","home":"hv1","subnet":"10.77.1.0/24"}`, "subnet is taken"},
	} {
		code, body := e.call(t, "POST", "/v1/networks", tokA, c.body)
		if code != 409 || !strings.Contains(body, c.want) {
			t.Errorf("%s = %d %s, want 409 %q", c.name, code, body, c.want)
		}
	}
	for _, c := range []struct{ name, body, field string }{
		{"no body fields", `{}`, "name"},
		{"empty name", `{"name":"","home":"hv1"}`, "name"},
		{"default", `{"name":"default","home":"hv1"}`, "name"},
		{"slash", `{"name":"a/b","home":"hv1"}`, "name"},
		{"long name", `{"name":"` + strings.Repeat("a", 64) + `","home":"hv1"}`, "name"},
		{"no home", `{"name":"net2"}`, "home"},
		{"home with a dot", `{"name":"net2","home":"hv1.lab"}`, "home"},
		{"domain", `{"name":"net2","home":"hv1","domain":"not a name"}`, "domain"},
		{"name that makes no domain", `{"name":"a_b","home":"hv1"}`, "domain"},
		{"subnet not cidr", `{"name":"net2","home":"hv1","subnet":"PLACEHOLDER-nope"}`, "subnet"},
		{"subnet /16", `{"name":"net2","home":"hv1","subnet":"10.77.0.0/16"}`, "subnet"},
		{"subnet unaligned", `{"name":"net2","home":"hv1","subnet":"10.77.2.5/24"}`, "subnet"},
		{"subnet v6", `{"name":"net2","home":"hv1","subnet":"fd00::/24"}`, "subnet"},
		{"vni zero", `{"name":"net2","home":"hv1","vni":0}`, "vni"},
		{"vni too big", `{"name":"net2","home":"hv1","vni":16777216}`, "vni"},
		{"vni negative", `{"name":"net2","home":"hv1","vni":-1}`, "vni"},
	} {
		code, body := e.call(t, "POST", "/v1/networks", tokA, c.body)
		if code != 400 || !strings.Contains(body, "invalid field: "+c.field) {
			t.Errorf("%s = %d %s, want 400 invalid field: %s", c.name, code, body, c.field)
		}
		if strings.Contains(body, "PLACEHOLDER-nope") || strings.Contains(body, "not a name") {
			t.Errorf("%s echoes the value: %s", c.name, body)
		}
	}
	// Refusals leave one network.
	if _, raw := e.call(t, "GET", "/v1/networks", tokA, ""); strings.Count(raw, `"name":"net`) != 1 {
		t.Errorf("after refusals: %s", raw)
	}
	// JSON only; a body over 1 MiB is 413.
	if code, _ := e.call(t, "POST", "/v1/networks", tokA, "{not json"); code != 400 {
		t.Errorf("bad json = %d, want 400", code)
	}
	big := `{"name":"net2","home":"hv1"}` + strings.Repeat(" ", 1<<20)
	if code, _ := e.call(t, "POST", "/v1/networks", tokA, big); code != 413 {
		t.Errorf("over 1 MiB = %d, want 413", code)
	}
	e.noSecretsInLogs(t)
}

func TestNetworkMemberRoutes(t *testing.T) {
	e := newInvEnv(t)
	e.newNet(t, `{"name":"net1","home":"home1"}`)
	if code, _ := e.call(t, "PUT", "/v1/networks/nope/members/hv1", tokA, `{"lan_ip":"192.0.2.11","lan_dev":"eth-test"}`); code != 404 {
		t.Errorf("member of a missing network = %d, want 404", code)
	}
	e.member(t, "net1", "hv1", "192.0.2.11")
	e.clock.Advance(time.Hour)
	// Again: new address, same joined time.
	code, raw := e.call(t, "PUT", "/v1/networks/net1/members/hv1", tokA, `{"lan_ip":"192.0.2.12","lan_dev":"eth-test"}`)
	var m storeapi.NetworkMember
	if code != 200 || json.Unmarshal([]byte(raw), &m) != nil || m.LanIp != "192.0.2.12" || m.Joined.Equal(e.clock.Now()) {
		t.Fatalf("re-add = %d %s", code, raw)
	}
	e.member(t, "net1", "hv2", "192.0.2.13")
	_, raw = e.call(t, "GET", "/v1/networks/net1", tokA, "")
	var n storeapi.Network
	if json.Unmarshal([]byte(raw), &n) != nil || len(n.Members) != 2 || n.Members[0].Host != "hv1" || n.Members[1].LanIp != "192.0.2.13" {
		t.Fatalf("members = %s", raw)
	}
	for _, c := range []struct{ name, path, body, field string }{
		{"host with a dot", "/v1/networks/net1/members/hv1.lab", `{"lan_ip":"192.0.2.11","lan_dev":"eth-test"}`, "host"},
		{"ip not an address", "/v1/networks/net1/members/hv1", `{"lan_ip":"PLACEHOLDER-nope","lan_dev":"eth-test"}`, "lan_ip"},
		{"ip v6", "/v1/networks/net1/members/hv1", `{"lan_ip":"2001:db8::1","lan_dev":"eth-test"}`, "lan_ip"},
		{"ip unspecified", "/v1/networks/net1/members/hv1", `{"lan_ip":"0.0.0.0","lan_dev":"eth-test"}`, "lan_ip"},
		{"ip loopback", "/v1/networks/net1/members/hv1", `{"lan_ip":"127.0.0.1","lan_dev":"eth-test"}`, "lan_ip"},
		{"dev empty", "/v1/networks/net1/members/hv1", `{"lan_ip":"192.0.2.11","lan_dev":""}`, "lan_dev"},
		{"dev too long", "/v1/networks/net1/members/hv1", `{"lan_ip":"192.0.2.11","lan_dev":"` + strings.Repeat("e", 16) + `"}`, "lan_dev"},
		{"dev with a slash", "/v1/networks/net1/members/hv1", `{"lan_ip":"192.0.2.11","lan_dev":"a/b"}`, "lan_dev"},
		{"no body", "/v1/networks/net1/members/hv1", `{}`, "lan_ip"},
	} {
		code, body := e.call(t, "PUT", c.path, tokA, c.body)
		if code != 400 || !strings.Contains(body, "invalid field: "+c.field) || strings.Contains(body, "PLACEHOLDER-nope") {
			t.Errorf("%s = %d %s, want 400 invalid field: %s", c.name, code, body, c.field)
		}
	}
	// A member blocks the delete; removing them all allows it.
	if code, body := e.call(t, "DELETE", "/v1/networks/net1", tokA, ""); code != 409 || !strings.Contains(body, "members") {
		t.Fatalf("delete with members = %d %s", code, body)
	}
	for _, h := range []string{"hv1", "hv2"} {
		if code, _ := e.call(t, "DELETE", "/v1/networks/net1/members/"+h, tokA, ""); code != 204 {
			t.Errorf("remove %s = %d, want 204", h, code)
		}
	}
	if code, _ := e.call(t, "DELETE", "/v1/networks/net1/members/hv1", tokA, ""); code != 404 {
		t.Errorf("remove again = %d, want 404", code)
	}
	if code, _ := e.call(t, "DELETE", "/v1/networks/net1", tokA, ""); code != 204 {
		t.Errorf("delete after members left = %d, want 204", code)
	}
	e.noSecretsInLogs(t)
}

// The host-token read: only the networks the host belongs to or is the home
// of, each with its peers' LAN addresses, and nothing else.
func TestHostNetworksRoute(t *testing.T) {
	e := newInvEnv(t)
	e.newNet(t, `{"name":"net1","home":"home1"}`)
	e.newNet(t, `{"name":"net2","home":"home1"}`)
	e.newNet(t, `{"name":"net3","home":"hv2"}`)
	e.member(t, "net1", "hv1", "192.0.2.11")
	e.member(t, "net1", "hv2", "192.0.2.12")
	e.member(t, "net1", "home1", "192.0.2.13")
	e.member(t, "net2", "hv2", "192.0.2.12")

	code, hn, raw := e.hostNets(t, "hv1", tokH)
	if code != 200 || hn.Host != "hv1" || len(hn.Networks) != 1 {
		t.Fatalf("hv1 = %d %s", code, raw)
	}
	n := hn.Networks[0]
	if n.Name != "net1" || n.Vni != 10000 || n.Bridge != "vmt-002710" || n.Home != "home1" || n.Subnet != "10.77.0.0/24" ||
		n.Member == nil || n.Member.LanIp != "192.0.2.11" || n.Member.LanDev != "eth-test" {
		t.Fatalf("hv1's network = %+v", n)
	}
	peers := map[string]string{}
	for _, p := range n.Peers {
		peers[p.Host] = p.LanIp
	}
	if len(peers) != 2 || peers["hv2"] != "192.0.2.12" || peers["home1"] != "192.0.2.13" {
		t.Errorf("peers = %v, want hv2 and home1 and not hv1 itself", peers)
	}
	// Nothing about networks hv1 is not part of, and no other host's interface.
	for _, leak := range []string{"net2", "net3", "10001", "10.77.1.0", "10.77.2.0"} {
		if strings.Contains(raw, leak) {
			t.Errorf("hv1's answer mentions %q: %s", leak, raw)
		}
	}
	// A peer carries a host and an address, nothing else.
	var generic struct {
		Networks []struct {
			Peers []map[string]any `json:"peers"`
		} `json:"networks"`
	}
	_ = json.Unmarshal([]byte(raw), &generic)
	for _, p := range generic.Networks[0].Peers {
		if len(p) != 2 {
			t.Errorf("peer = %v, want only host and lan_ip", p)
		}
	}

	// hv2: a member of net1 and net2, and the home of net3 (not a member).
	code, hn, raw = e.hostNets(t, "hv2", tokH2)
	if code != 200 || len(hn.Networks) != 3 || hn.Networks[2].Name != "net3" || hn.Networks[2].Member != nil || len(hn.Networks[2].Peers) != 0 {
		t.Fatalf("hv2 = %d %s", code, raw)
	}

	// Another host's name with a host token is 403; so is the same with hv2's.
	if code, _, body := e.hostNets(t, "hv2", tokH); code != 403 || strings.Contains(body, "net1") {
		t.Errorf("hv1's token on hv2's networks = %d %s, want 403", code, body)
	}
	if code, _, _ := e.hostNets(t, "hv1", tokH2); code != 403 {
		t.Errorf("hv2's token on hv1's networks = %d, want 403", code)
	}
	// A user token may name any host; a host with none gets an empty list.
	if code, hn, _ := e.hostNets(t, "hv1", tokA); code != 200 || len(hn.Networks) != 1 {
		t.Errorf("user token = %d %+v", code, hn)
	}
	if code, hn, raw := e.hostNets(t, "hv9", tokA); code != 200 || hn.Networks == nil || len(hn.Networks) != 0 || !strings.Contains(raw, `"networks":[]`) {
		t.Errorf("host with none = %d %s", code, raw)
	}
	if code, _, _ := e.hostNets(t, "hv1.lab", tokA); code != 400 {
		t.Errorf("bad host = %d, want 400", code)
	}
	// Bad and missing tokens.
	if code, _, _ := e.hostNets(t, "hv1", "PLACEHOLDER-wrong"); code != 401 {
		t.Errorf("wrong token = %d, want 401", code)
	}
	// No up/down state anywhere.
	for _, w := range []string{"\"up\"", "\"down\"", "\"online\"", "\"status\"", "\"joined\""} {
		if strings.Contains(raw, w) {
			t.Errorf("host networks carry %s", w)
		}
	}
	e.noSecretsInLogs(t)
}

// A host token reaches the one read and none of the other network routes
// (read or write); the allow-list over every spec route is in
// TestHostTokenAllowList.
func TestHostTokenCannotUseUserNetworkRoutes(t *testing.T) {
	e := newInvEnv(t)
	e.newNet(t, `{"name":"net1","home":"hv1"}`)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/networks", ""},
		{"POST", "/v1/networks", `{"name":"net2","home":"hv1"}`},
		{"GET", "/v1/networks/net1", ""},
		{"DELETE", "/v1/networks/net1", ""},
		{"PUT", "/v1/networks/net1/members/hv1", `{"lan_ip":"192.0.2.11","lan_dev":"eth-test"}`},
		{"DELETE", "/v1/networks/net1/members/hv1", ""},
	} {
		if code, body := e.call(t, c.method, c.path, tokH, c.body); code != 401 {
			t.Errorf("host token on %s %s = %d %s, want 401", c.method, c.path, code, body)
		}
	}
	if _, raw := e.call(t, "GET", "/v1/networks", tokA, ""); !strings.Contains(raw, `"net1"`) || strings.Contains(raw, "net2") || strings.Contains(raw, `"members":[{`) {
		t.Errorf("a refused host token changed the registry: %s", raw)
	}
}

// Concurrent creates over HTTP never hand out the same VNI or subnet.
func TestConcurrentNetworkCreatesOverHTTP(t *testing.T) {
	e := newInvEnv(t)
	const n = 16
	var wg sync.WaitGroup
	nets := make([]storeapi.Network, n)
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, raw := e.call(t, "POST", "/v1/networks", tokA, fmt.Sprintf(`{"name":"net%d","home":"home1"}`, i))
			codes[i] = code
			_ = json.Unmarshal([]byte(raw), &nets[i])
		}()
	}
	wg.Wait()
	vnis, subnets := map[int]bool{}, map[string]bool{}
	for i := 0; i < n; i++ {
		if codes[i] != 201 {
			t.Fatalf("create %d = %d", i, codes[i])
		}
		if vnis[nets[i].Vni] || subnets[nets[i].Subnet] {
			t.Errorf("duplicate VNI or subnet: %+v", nets[i])
		}
		vnis[nets[i].Vni], subnets[nets[i].Subnet] = true, true
	}
	e.noSecretsInLogs(t)
}

// With no database the routes answer the fixed 500, like the others.
func TestNetworkRoutesWithoutDatabase(t *testing.T) {
	ts, _ := testServerNoDB(t)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/networks", ""},
		{"POST", "/v1/networks", `{"name":"net1","home":"hv1"}`},
		{"GET", "/v1/networks/net1", ""},
		{"DELETE", "/v1/networks/net1", ""},
		{"PUT", "/v1/networks/net1/members/hv1", `{"lan_ip":"192.0.2.11","lan_dev":"eth-test"}`},
		{"DELETE", "/v1/networks/net1/members/hv1", ""},
		{"GET", "/v1/hosts/hv1/networks", ""},
	} {
		req, _ := newJSONReq(c.method, ts.URL+c.path, tokA, c.body)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 500 {
			t.Errorf("%s %s with no database = %d, want 500", c.method, c.path, resp.StatusCode)
		}
	}
}
