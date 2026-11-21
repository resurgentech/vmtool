package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/api"
)

// N9: GET /v1/inventory on the workstation reads the store through
// app.Service.Inventory; no store or a store that does not answer is 503 with
// the app's text; a hypervisor refuses it (TestWorkstationOnlySet and
// TestWorkstationOnlyRefusedByHypervisor cover the 403).

func TestGetInventoryReadsTheStore(t *testing.T) {
	s := newSrvStore(t)
	h := delistHandler(t, s, &vmMgr{}, true)
	rec := do(h, "GET", "/v1/inventory?host=hv1&gone=true", "")
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	var inv api.Inventory
	if err := json.Unmarshal(rec.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 1 || inv.Hosts[0].Name != "hv1" || inv.Hosts[0].Live != 1 || inv.Hosts[0].Gone != 1 ||
		inv.Hosts[0].DaemonVersion != "2.6.0" || inv.Hosts[0].LastReport.Format("2006-01-02T15:04:05Z") != "2026-11-21T01:02:03Z" {
		t.Errorf("hosts %+v", inv.Hosts)
	}
	if len(inv.Vms) != 2 || len(inv.Machines) != 1 || inv.Machines[0].Name != "nuc1" || inv.Machines[0].Kind != "metal" {
		t.Fatalf("vms/machines %+v", inv)
	}
	v := inv.Vms[0]
	if v.Host != "hv1" || v.Name != "vm1" || v.State != "running" || v.Vcpus != 2 || v.Memory != 4 || v.Ip != "10.0.0.5" ||
		!v.Autostart || v.Comment != "placeholder comment" || !v.HasCredentials || len(v.Disks) != 1 ||
		v.Disks[0].Kind != "thin" || v.Disks[0].CapacityBytes != 21474836480 || v.GoneSince != nil {
		t.Errorf("vm1 %+v", v)
	}
	if v.Origin == nil || *v.Origin.Image != "img1" || *v.Origin.Clone != "thin" || *v.Origin.ImageSha256 != strings.Repeat("a", 64) {
		t.Errorf("origin %+v", v.Origin)
	}
	if v.DisksUnreadable == nil || !*v.DisksUnreadable {
		t.Errorf("vm1 lacks the disks_unreadable flag: %+v", v)
	}
	g := inv.Vms[1]
	if g.DisksUnreadable != nil {
		t.Errorf("vm2 carries the flag: %+v", g)
	}
	if g.GoneSince == nil || g.CredentialsKeptUntil == nil || g.CredentialsKeptUntil.Format("2006-01-02") != "2026-11-28" || g.Origin != nil {
		t.Errorf("gone vm %+v", g)
	}
	if q, _ := s.invQuery.Load().(string); q != "/v1/inventory?gone=true&host=hv1" {
		t.Errorf("store saw %q", q)
	}
	if got := rec.Header().Values(warnHeader); len(got) != 0 {
		t.Errorf("warnings %v on a store that answered", got)
	}
	if strings.Contains(rec.Body.String(), srvStoreTok) || strings.Contains(fmt.Sprint(rec.Header()), srvStoreTok) {
		t.Error("the store token is in the response")
	}
}

func TestGetInventoryWithoutFilters(t *testing.T) {
	s := newSrvStore(t)
	h := delistHandler(t, s, &vmMgr{}, true)
	if rec := do(h, "GET", "/v1/inventory", ""); rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if q, _ := s.invQuery.Load().(string); q != "/v1/inventory" {
		t.Errorf("store saw %q", q)
	}
}

// No store configured, or one that does not answer: 503 and the same text the
// CLI prints; no fallback warning (there is no local inventory to fall back to).
func TestGetInventoryUnavailable(t *testing.T) {
	s := newSrvStore(t)
	rec := do(delistHandler(t, s, &vmMgr{}, false), "GET", "/v1/inventory", "")
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "no store is configured") {
		t.Fatalf("no store: %d %s", rec.Code, rec.Body)
	}
	if n := s.reqs.Load(); n != 0 {
		t.Errorf("store requests %d with none configured", n)
	}

	s.down.Store(true)
	rec = do(delistHandler(t, s, &vmMgr{}, true), "GET", "/v1/inventory", "")
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "is not reachable") {
		t.Fatalf("store down: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Values(warnHeader); len(got) != 0 {
		t.Errorf("warnings %v", got)
	}
	var e api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || !strings.HasPrefix(e.Error, "inventory: ") {
		t.Errorf("error body %q (%v)", rec.Body, err)
	}
	if strings.Contains(rec.Body.String(), srvStoreTok) || strings.Contains(fmt.Sprint(rec.Header()), srvStoreTok) {
		t.Error("the store token is in the response")
	}
}
