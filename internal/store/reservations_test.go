package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/storeapi"
)

// V5: the store records each spanning network's reservations (network, MAC,
// address, host, VM) when a client makes and removes them. User tokens only.

func (e *invEnv) reservations(t *testing.T, network string) []storeapi.NetworkReservation {
	t.Helper()
	code, raw := e.call(t, "GET", "/v1/networks/"+network+"/reservations", tokA, "")
	if code != 200 {
		t.Fatalf("list reservations %s = %d %s", network, code, raw)
	}
	var l storeapi.NetworkReservationList
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatal(err)
	}
	return l.Reservations
}

func TestNetworkReservationsRoutes(t *testing.T) {
	e := newInvEnv(t)
	e.newNet(t, `{"name":"net1","home":"home1"}`)
	e.newNet(t, `{"name":"net2","home":"home1"}`)

	if got := e.reservations(t, "net1"); got == nil || len(got) != 0 {
		t.Fatalf("a new network's reservations = %#v, want an empty non-nil list", got)
	}
	put := func(net, mac, body string) (int, string) {
		return e.call(t, "PUT", "/v1/networks/"+net+"/reservations/"+mac, tokA, body)
	}
	// Recorded out of address order; listed in address order (10 after 9).
	if code, raw := put("net1", "52:54:00:00:00:02", `{"ip":"10.77.0.110","host":"hv2","vm":"web2"}`); code != 200 || !strings.Contains(raw, `"vm":"web2"`) {
		t.Fatalf("put = %d %s", code, raw)
	}
	if code, raw := put("net1", "52:54:00:00:00:01", `{"ip":"10.77.0.109","host":"hv1","vm":"web1"}`); code != 200 {
		t.Fatalf("put = %d %s", code, raw)
	}
	got := e.reservations(t, "net1")
	if len(got) != 2 || got[0].Ip != "10.77.0.109" || got[0].Mac != "52:54:00:00:00:01" || got[0].Host != "hv1" || got[0].Vm != "web1" || got[0].Network != "net1" ||
		got[1].Ip != "10.77.0.110" {
		t.Fatalf("list = %+v", got)
	}
	if other := e.reservations(t, "net2"); len(other) != 0 {
		t.Errorf("net2 sees net1's reservations: %+v", other)
	}

	// Idempotent: the same MAC again takes the new values (a VM moved).
	if code, _ := put("net1", "52:54:00:00:00:01", `{"ip":"10.77.0.109","host":"hv2","vm":"web1"}`); code != 200 {
		t.Fatalf("again = %d", code)
	}
	if got := e.reservations(t, "net1"); len(got) != 2 || got[0].Host != "hv2" {
		t.Errorf("after a second put: %+v", got)
	}
	// An upper-case MAC is the same MAC.
	if code, _ := put("net1", "52:54:00:00:00:0A", `{"ip":"10.77.0.111","host":"hv1","vm":"web3"}`); code != 200 {
		t.Fatalf("upper = %d", code)
	}
	if got := e.reservations(t, "net1"); got[2].Mac != "52:54:00:00:00:0a" {
		t.Errorf("MAC not lower-cased: %+v", got)
	}

	// An address another MAC holds is a conflict; nothing changes.
	if code, _ := put("net1", "52:54:00:00:00:03", `{"ip":"10.77.0.109","host":"hv1","vm":"web4"}`); code != 409 {
		t.Errorf("taken address = %d, want 409", code)
	}
	if got := e.reservations(t, "net1"); len(got) != 3 {
		t.Errorf("after a refused put: %+v", got)
	}
	// The same address on another network is another matter.
	if code, _ := put("net2", "52:54:00:00:00:03", `{"ip":"10.77.0.109","host":"hv1","vm":"web4"}`); code != 200 {
		t.Errorf("same address on net2 = %d, want 200", code)
	}

	if code, _ := put("nonet", "52:54:00:00:00:01", `{"ip":"10.77.0.109","host":"hv1","vm":"web1"}`); code != 404 {
		t.Errorf("unknown network = %d, want 404", code)
	}
	if code, _ := e.call(t, "GET", "/v1/networks/nonet/reservations", tokA, ""); code != 404 {
		t.Errorf("list of an unknown network = %d, want 404", code)
	}

	for _, c := range []struct{ name, mac, body, field string }{
		{"mac", "not-a-mac", `{"ip":"10.77.0.109","host":"hv1","vm":"web1"}`, "mac"},
		{"mac short", "52:54:00:00:00", `{"ip":"10.77.0.109","host":"hv1","vm":"web1"}`, "mac"},
		{"ip", "52:54:00:00:00:07", `{"ip":"PLACEHOLDER-nope","host":"hv1","vm":"web1"}`, "ip"},
		{"ip v6", "52:54:00:00:00:07", `{"ip":"2001:db8::1","host":"hv1","vm":"web1"}`, "ip"},
		{"host", "52:54:00:00:00:07", `{"ip":"10.77.0.150","host":"hv1.lab","vm":"web1"}`, "host"},
		{"vm empty", "52:54:00:00:00:07", `{"ip":"10.77.0.150","host":"hv1","vm":""}`, "vm"},
		{"vm bad", "52:54:00:00:00:07", `{"ip":"10.77.0.150","host":"hv1","vm":"a b"}`, "vm"},
		{"no body", "52:54:00:00:00:07", `{}`, "ip"},
	} {
		code, body := put("net1", c.mac, c.body)
		if code != 400 || !strings.Contains(body, "invalid field: "+c.field) || strings.Contains(body, "PLACEHOLDER-nope") {
			t.Errorf("%s = %d %s, want 400 invalid field: %s", c.name, code, body, c.field)
		}
	}

	del := func(net, mac string) int {
		code, _ := e.call(t, "DELETE", "/v1/networks/"+net+"/reservations/"+mac, tokA, "")
		return code
	}
	if code := del("net1", "52:54:00:00:00:0A"); code != 204 {
		t.Errorf("delete (upper-case) = %d, want 204", code)
	}
	if code := del("net1", "52:54:00:00:00:0a"); code != 404 {
		t.Errorf("delete again = %d, want 404", code)
	}
	if code := del("net1", "garbage"); code != 400 {
		t.Errorf("delete bad mac = %d, want 400", code)
	}
	if got := e.reservations(t, "net1"); len(got) != 2 {
		t.Errorf("after delete: %+v", got)
	}

	// Deleting the network (no members) takes its records with it: a network
	// made again under the name starts empty.
	if code, _ := e.call(t, "DELETE", "/v1/networks/net1", tokA, ""); code != 204 {
		t.Fatalf("delete network = %d", code)
	}
	e.newNet(t, `{"name":"net1","home":"home1"}`)
	if got := e.reservations(t, "net1"); len(got) != 0 {
		t.Errorf("a re-made network inherited reservations: %+v", got)
	}
	e.noSecretsInLogs(t)
}

// A host token has none of these routes (the allow-list names its three).
func TestNetworkReservationsRefuseAHostToken(t *testing.T) {
	e := newInvEnv(t)
	e.newNet(t, `{"name":"net1","home":"home1"}`)
	for _, r := range [][2]string{
		{"GET", "/v1/networks/net1/reservations"},
		{"PUT", "/v1/networks/net1/reservations/52:54:00:00:00:01"},
		{"DELETE", "/v1/networks/net1/reservations/52:54:00:00:00:01"},
		{"GET", "/v1/networks/net1/reservations/orphaned"},
		{"POST", "/v1/networks/net1/reservations/52:54:00:00:00:01/orphan"},
	} {
		if code, _ := e.call(t, r[0], r[1], tokH, `{"ip":"10.77.0.109","host":"hv1","vm":"web1"}`); code != 401 {
			t.Errorf("%s %s with a host token = %d, want 401", r[0], r[1], code)
		}
	}
}

// G3: a record carries the VM's UUID; a user token marks one orphaned and lists
// a network's orphans; nothing is orphaned by listing.
func TestOrphanedReservationRoutes(t *testing.T) {
	e := newInvEnv(t)
	e.newNet(t, `{"name":"net1","home":"home1"}`)
	const u1 = "00000000-0000-4000-8000-000000000001"
	put := func(mac, ip, vm, extra string) (int, string) {
		return e.call(t, "PUT", "/v1/networks/net1/reservations/"+mac, tokA, `{"ip":"`+ip+`","host":"hv1","vm":"`+vm+`"`+extra+`}`)
	}
	if code, raw := put("52:54:00:00:00:01", "10.77.0.100", "web1", `,"uuid":"`+u1+`"`); code != 200 || !strings.Contains(raw, `"uuid":"`+u1+`"`) || strings.Contains(raw, "orphaned_at") {
		t.Fatalf("put = %d %s", code, raw)
	}
	if code, _ := put("52:54:00:00:00:02", "10.77.0.101", "web2", ``); code != 200 {
		t.Fatalf("put 2 = %d", code)
	}
	orphans := func() []storeapi.NetworkReservation {
		t.Helper()
		code, raw := e.call(t, "GET", "/v1/networks/net1/reservations/orphaned", tokA, "")
		if code != 200 {
			t.Fatalf("list orphans = %d %s", code, raw)
		}
		var l storeapi.NetworkReservationList
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatal(err)
		}
		if l.Reservations == nil {
			t.Fatalf("orphans = null, want an empty list: %s", raw)
		}
		return l.Reservations
	}
	if got := orphans(); len(got) != 0 {
		t.Fatalf("orphans before any mark: %+v", got)
	}
	code, raw := e.call(t, "POST", "/v1/networks/net1/reservations/52:54:00:00:00:01/orphan", tokA, "")
	if code != 200 || !strings.Contains(raw, `"orphaned_at"`) {
		t.Fatalf("mark = %d %s", code, raw)
	}
	got := orphans()
	if len(got) != 1 || got[0].Mac != "52:54:00:00:00:01" || got[0].OrphanedAt == nil || got[0].Uuid == nil || *got[0].Uuid != u1 {
		t.Fatalf("orphans = %+v", got)
	}
	if all := e.reservations(t, "net1"); len(all) != 2 {
		t.Errorf("the full list = %+v, want both records", all)
	}
	if code, _ := e.call(t, "POST", "/v1/networks/net1/reservations/52:54:00:00:00:09/orphan", tokA, ""); code != 404 {
		t.Errorf("mark of a missing record = %d, want 404", code)
	}
	if code, _ := e.call(t, "POST", "/v1/networks/net1/reservations/garbage/orphan", tokA, ""); code != 400 {
		t.Errorf("mark with a bad MAC = %d, want 400", code)
	}
	if code, _ := e.call(t, "GET", "/v1/networks/nonet/reservations/orphaned", tokA, ""); code != 404 {
		t.Errorf("orphans of an unknown network = %d, want 404", code)
	}
	if code, _ := e.call(t, "POST", "/v1/networks/nonet/reservations/52:54:00:00:00:01/orphan", tokA, ""); code != 404 {
		t.Errorf("mark on an unknown network = %d, want 404", code)
	}
	e.noSecretsInLogs(t)
}

// A host token can neither mark nor list orphans, with a real token of a real host.
func TestHostTokenCannotMarkOrListOrphans(t *testing.T) {
	ts, logs := hostServer(t)
	for _, r := range [][2]string{
		{"GET", "/v1/networks/PLACEHOLDER-name/reservations/orphaned"},
		{"POST", "/v1/networks/PLACEHOLDER-name/reservations/52:54:00:00:00:01/orphan"},
	} {
		if resp, body := do(t, r[0], ts.URL+r[1], "Bearer "+tokH, ""); resp.StatusCode != 401 || strings.Contains(body, tokH) {
			t.Errorf("%s %s with a host token = %d, want 401", r[0], r[1], resp.StatusCode)
		}
	}
	if strings.Contains(logs.String(), tokH) {
		t.Errorf("log carries the host token")
	}
}
