package store

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/store/blob"
	"github.com/jaredeh/vmtool/internal/store/db"
	"github.com/jaredeh/vmtool/internal/storeapi"
	"github.com/jaredeh/vmtool/internal/tokenhash"
)

const (
	tokH2  = "PLACEHOLDER-not-a-secret-4" // host token of hv2
	invU1  = "00000000-0000-4000-8000-000000000001"
	invU2  = "00000000-0000-4000-8000-000000000002"
	invU3  = "00000000-0000-4000-8000-000000000003"
	invSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// invClock is the store's injected clock; nothing in these tests sleeps.
type invClock struct {
	mu sync.Mutex
	t  time.Time
}

func newInvClock() *invClock { return &invClock{t: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)} }

func (c *invClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *invClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type invEnv struct {
	ts    *httptest.Server
	logs  *bytes.Buffer
	db    *db.DB
	clock *invClock
}

func newInvEnv(t *testing.T) *invEnv {
	t.Helper()
	e := &invEnv{logs: &bytes.Buffer{}, db: openTestDB(t), clock: newInvClock()}
	c := &Config{
		Listen:     "127.0.0.1:8095",
		Tokens:     map[string]string{"alice": tokenhash.Hash(tokA)},
		HostTokens: map[string]string{"hv1": tokenhash.Hash(tokH), "hv2": tokenhash.Hash(tokH2)},
	}
	s := NewServer(c, slog.New(slog.NewJSONHandler(e.logs, nil)), Deps{Blob: blob.NewMemory(), DB: e.db, Now: e.clock.Now})
	e.ts = httptest.NewServer(s.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func (e *invEnv) call(t *testing.T, method, path, tok, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func ph(s string) *string { return &s }

func vm(name, uuid string) storeapi.ReportVM {
	return storeapi.ReportVM{
		Name: name, Uuid: uuid, State: "running", Vcpus: 2, Memory: 4, Ip: "192.0.2.10", Firmware: "uefi",
		Disks: []storeapi.Disk{{Target: "vda", Pool: "default", CapacityBytes: 1 << 30, Kind: storeapi.File}},
	}
}

func reportBody(t *testing.T, at time.Time, vms ...storeapi.ReportVM) string {
	t.Helper()
	if vms == nil {
		vms = []storeapi.ReportVM{}
	}
	b, err := json.Marshal(storeapi.HostReport{GeneratedAt: at, DaemonVersion: "2.6.0", Vms: vms})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (e *invEnv) report(t *testing.T, host, tok string, vms ...storeapi.ReportVM) storeapi.ReportResult {
	t.Helper()
	code, body := e.call(t, "PUT", "/v1/hosts/"+host+"/inventory", tok, reportBody(t, e.clock.Now(), vms...))
	if code != 200 {
		t.Fatalf("report = %d %s", code, body)
	}
	var r storeapi.ReportResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *invEnv) inventory(t *testing.T, query string) storeapi.Inventory {
	t.Helper()
	code, body := e.call(t, "GET", "/v1/inventory"+query, tokA, "")
	if code != 200 {
		t.Fatalf("inventory%s = %d %s", query, code, body)
	}
	var inv storeapi.Inventory
	if err := json.Unmarshal([]byte(body), &inv); err != nil {
		t.Fatal(err)
	}
	return inv
}

func (e *invEnv) noSecretsInLogs(t *testing.T) {
	t.Helper()
	for _, s := range []string{tokA, tokH, tokH2, tokenhash.Hash(tokH), tokenhash.Hash(tokH2)} {
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("log carries %q", s)
		}
	}
}

func TestReportHostTokenForThatHostOnly(t *testing.T) {
	e := newInvEnv(t)
	r := e.report(t, "hv1", tokH, vm("vm1", invU1), vm("vm2", invU2))
	if r.Listed != 2 || r.NewlyGone != 0 || r.Relisted != 0 || r.Stale {
		t.Fatalf("counts = %+v", r)
	}
	body := reportBody(t, e.clock.Now(), vm("vm1", invU1))
	for _, c := range []struct{ name, path, tok string }{
		{"hv1 token on hv2", "/v1/hosts/hv2/inventory", tokH},
		{"hv2 token on hv1", "/v1/hosts/hv1/inventory", tokH2},
		{"user token", "/v1/hosts/hv1/inventory", tokA},
	} {
		code, resp := e.call(t, "PUT", c.path, c.tok, body)
		if code != 403 {
			t.Errorf("%s = %d, want 403", c.name, code)
		}
		if strings.Contains(resp, c.tok) {
			t.Errorf("%s echoes the token", c.name)
		}
	}
	// The refused reports changed nothing.
	if inv := e.inventory(t, ""); len(inv.Vms) != 2 || len(inv.Hosts) != 1 {
		t.Fatalf("inventory after refused reports = %+v", inv)
	}
	e.noSecretsInLogs(t)
}

func TestReportValidation(t *testing.T) {
	e := newInvEnv(t)
	at := e.clock.Now()
	tooMany := make([]storeapi.ReportVM, 4097)
	for i := range tooMany {
		tooMany[i] = vm("vm1", invU1)
	}
	mod := func(f func(*storeapi.ReportVM)) string {
		v := vm("vm1", invU1)
		f(&v)
		return reportBody(t, at, v)
	}
	cases := []struct {
		name, body, field string
	}{
		{"bad uuid", mod(func(v *storeapi.ReportVM) { v.Uuid = "PLACEHOLDER-not-a-uuid" }), "uuid"},
		{"name with a slash", mod(func(v *storeapi.ReportVM) { v.Name = "a/b" }), "name"},
		{"empty name", mod(func(v *storeapi.ReportVM) { v.Name = "" }), "name"},
		{"name control", mod(func(v *storeapi.ReportVM) { v.Name = "a\tb" }), "name"},
		{"empty state", mod(func(v *storeapi.ReportVM) { v.State = "" }), "state"},
		{"negative vcpus", mod(func(v *storeapi.ReportVM) { v.Vcpus = -1 }), "vcpus"},
		{"bad disk kind", mod(func(v *storeapi.ReportVM) { v.Disks[0].Kind = "tape" }), "disks"},
		{"too many vms", reportBody(t, at, tooMany...), "vms"},
		{"network name control", mod(func(v *storeapi.ReportVM) {
			v.Networks = &[]storeapi.VMNetwork{{Network: "net\t1", Mac: "52:54:00:00:00:01", Ip: "10.77.1.100"}}
		}), "networks"},
		{"network mac too long", mod(func(v *storeapi.ReportVM) {
			v.Networks = &[]storeapi.VMNetwork{{Network: "net1", Mac: strings.Repeat("a", 200), Ip: "10.77.1.100"}}
		}), "networks"},
		{"too many networks", mod(func(v *storeapi.ReportVM) {
			n := make([]storeapi.VMNetwork, 65)
			for i := range n {
				n[i] = storeapi.VMNetwork{Network: "net1", Mac: "52:54:00:00:00:01", Ip: "10.77.1.100"}
			}
			v.Networks = &n
		}), "networks"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := e.call(t, "PUT", "/v1/hosts/hv1/inventory", tokH, c.body)
			if code != 400 || !strings.Contains(body, "invalid field: "+c.field) {
				t.Fatalf("= %d %s, want 400 naming %q", code, body, c.field)
			}
		})
	}
	if inv := e.inventory(t, "?gone=true"); len(inv.Vms) != 0 || len(inv.Hosts) != 0 {
		t.Fatalf("a refused report changed the inventory: %+v", inv)
	}

	// A VM libvirt allows but vmtool would not name (made outside vmtool)
	// must not cost the host its whole report.
	odd := vm("vm1 copy+2:x", invU1)
	if code, body := e.call(t, "PUT", "/v1/hosts/hv1/inventory", tokH, reportBody(t, at, odd)); code != 200 {
		t.Fatalf("report with an outside-vmtool name = %d %s, want 200", code, body)
	}
	if inv := e.inventory(t, ""); len(inv.Vms) != 1 || inv.Vms[0].Name != odd.Name {
		t.Fatalf("inventory after odd name = %+v", inv)
	}

	// A report with several VMs is accepted.
	e.report(t, "hv1", tokH, vm("vm1", invU1), vm("vm2", invU2), vm("vm3", invU3))
	// A report carrying the removed origin and comment fields (a daemon built
	// before they left the report) is still taken; the fields are ignored.
	old := strings.Replace(reportBody(t, at, vm("vm1", invU1)), `"name":"vm1"`,
		`"name":"vm1","comment":"placeholder comment","origin":{"image":"img1","clone":"file"}`, 1)
	if code, body := e.call(t, "PUT", "/v1/hosts/hv1/inventory", tokH, old); code != 200 {
		t.Fatalf("report with the old fields = %d %s", code, body)
	}
	if v := e.inventory(t, "").Vms[0]; v.Comment != "" || v.Origin != nil {
		t.Fatalf("a report set notes: %+v", v)
	}

	// Not JSON; bad JSON; a declared body over 1 MiB.
	req, _ := http.NewRequest("PUT", e.ts.URL+"/v1/hosts/hv1/inventory", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tokH)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("text/plain = %d, want 400", resp.StatusCode)
	}
	if code, _ := e.call(t, "PUT", "/v1/hosts/hv1/inventory", tokH, "{not json"); code != 400 {
		t.Errorf("bad json = %d, want 400", code)
	}
	big := reportBody(t, at, vm("vm1", invU1)) + strings.Repeat(" ", 1<<20)
	if code, _ := e.call(t, "PUT", "/v1/hosts/hv1/inventory", tokH, big); code != 413 {
		t.Errorf("over 1 MiB = %d, want 413", code)
	}
	e.noSecretsInLogs(t)
}

func TestStaleReportAnswered200(t *testing.T) {
	e := newInvEnv(t)
	e.clock.Advance(time.Hour)
	e.report(t, "hv1", tokH, vm("vm1", invU1))
	// Ordered by the store's clock: a report taken before the applied one is
	// stale, whatever its generated_at says.
	e.clock.Advance(-time.Minute)
	old := reportBody(t, e.clock.Now().Add(24*time.Hour))
	code, body := e.call(t, "PUT", "/v1/hosts/hv1/inventory", tokH, old)
	var r storeapi.ReportResult
	if code != 200 || json.Unmarshal([]byte(body), &r) != nil || !r.Stale {
		t.Fatalf("stale report = %d %s", code, body)
	}
	if inv := e.inventory(t, ""); len(inv.Vms) != 1 {
		t.Fatalf("a stale empty report changed the inventory: %+v", inv)
	}
}

func TestReportGeneratedAtDoesNotOrderReports(t *testing.T) {
	e := newInvEnv(t)
	year := 365 * 24 * time.Hour
	body := func(at time.Time, vms ...storeapi.ReportVM) string { return reportBody(t, at, vms...) }
	put := func(b string) storeapi.ReportResult {
		code, resp := e.call(t, "PUT", "/v1/hosts/hv1/inventory", tokH, b)
		var r storeapi.ReportResult
		if code != 200 || json.Unmarshal([]byte(resp), &r) != nil {
			t.Fatalf("report = %d %s", code, resp)
		}
		return r
	}
	if r := put(body(e.clock.Now().Add(year), vm("vm1", invU1))); r.Stale || r.Listed != 1 {
		t.Fatalf("a year ahead = %+v", r)
	}
	e.clock.Advance(time.Minute)
	if r := put(body(e.clock.Now(), vm("vm1", invU1), vm("vm2", invU2))); r.Stale || r.Listed != 2 {
		t.Fatalf("honest after a year ahead = %+v", r)
	}
	e.clock.Advance(time.Minute)
	if r := put(body(e.clock.Now().Add(-year), vm("vm1", invU1))); r.Stale || r.NewlyGone != 1 {
		t.Fatalf("a year behind = %+v", r)
	}
	inv := e.inventory(t, "")
	if len(inv.Vms) != 1 || !inv.Hosts[0].LastReport.Equal(e.clock.Now()) {
		t.Fatalf("inventory = %+v", inv)
	}
}

// V2: a report carries each VM's cards on vmtool networks; the inventory shows
// what the last report said, and omits the field for a VM with none.
func TestReportNetworksAreStoredAndShown(t *testing.T) {
	e := newInvEnv(t)
	a := vm("vm1", invU1)
	a.Networks = &[]storeapi.VMNetwork{{Network: "net1", Mac: "52:54:00:00:00:01", Ip: "10.77.1.100"}}
	e.report(t, "hv1", tokH, a, vm("vm2", invU2))
	by := map[string]storeapi.InventoryVM{}
	for _, v := range e.inventory(t, "").Vms {
		by[v.Name] = v
	}
	if n := by["vm1"].Networks; n == nil || len(*n) != 1 || (*n)[0].Network != "net1" || (*n)[0].Mac != "52:54:00:00:00:01" || (*n)[0].Ip != "10.77.1.100" {
		t.Fatalf("vm1 networks = %+v", n)
	}
	if by["vm2"].Networks != nil {
		t.Fatalf("vm2 networks = %+v, want omitted", by["vm2"].Networks)
	}
	e.clock.Advance(time.Minute)
	e.report(t, "hv1", tokH, vm("vm1", invU1), vm("vm2", invU2))
	for _, v := range e.inventory(t, "").Vms {
		if v.Networks != nil {
			t.Fatalf("%s still has networks after a report with none: %+v", v.Name, v.Networks)
		}
	}
}

func TestReportDisksUnreadableMarksAndKeepsDisks(t *testing.T) {
	e := newInvEnv(t)
	e.report(t, "hv1", tokH, vm("vm1", invU1), vm("vm2", invU2))
	e.clock.Advance(time.Minute)
	u := vm("vm1", invU1)
	u.State = "shutoff"
	u.Disks = []storeapi.Disk{}
	yes := true
	u.DisksUnreadable = &yes
	r := e.report(t, "hv1", tokH, u, vm("vm2", invU2))
	if r.Listed != 2 || r.NewlyGone != 0 {
		t.Fatalf("counts = %+v", r)
	}
	inv := e.inventory(t, "")
	by := map[string]storeapi.InventoryVM{}
	for _, v := range inv.Vms {
		by[v.Name] = v
	}
	v1, v2 := by["vm1"], by["vm2"]
	if v1.DisksUnreadable == nil || !*v1.DisksUnreadable || len(v1.Disks) != 1 || v1.Disks[0].Target != "vda" || v1.State != "shutoff" {
		t.Fatalf("vm1 = %+v", v1)
	}
	if v2.DisksUnreadable != nil && *v2.DisksUnreadable {
		t.Fatalf("vm2 marked: %+v", v2)
	}
	e.clock.Advance(time.Minute)
	e.report(t, "hv1", tokH, vm("vm1", invU1), vm("vm2", invU2))
	for _, v := range e.inventory(t, "").Vms {
		if v.DisksUnreadable != nil && *v.DisksUnreadable {
			t.Fatalf("%s still marked", v.Name)
		}
	}
}

func TestInventoryReadRoute(t *testing.T) {
	e := newInvEnv(t)
	v := vm("vm1", invU1)
	// The notes come from the notes route, not from the report.
	if code, _, body := e.notes(t, "hv1", "vm1", `{"uuid":"`+invU1+`","origin":{"image":"img1","image_sha256":"`+invSHA+
		`","clone":"file","pool":"default","created":"2026-11-20T00:00:00Z"},"comment":"placeholder comment"}`); code != 200 {
		t.Fatalf("notes = %d %s", code, body)
	}
	e.report(t, "hv2", tokH2, vm("vm9", invU3), vm("vm2", invU2))
	e.report(t, "hv1", tokH, v)
	if code, _ := e.call(t, "PUT", "/v1/credentials/machines/m1", tokA,
		`{"kind":"metal","address":"192.0.2.7","username":"PLACEHOLDER-user"}`); code != 200 {
		t.Fatalf("put machine = %d", code)
	}

	inv := e.inventory(t, "")
	if len(inv.Hosts) != 2 || inv.Hosts[0].Name != "hv1" || inv.Hosts[1].Name != "hv2" {
		t.Fatalf("hosts = %+v", inv.Hosts)
	}
	if h := inv.Hosts[0]; h.Live != 1 || h.Gone != 0 || h.DaemonVersion != "2.6.0" || !h.LastReport.Equal(e.clock.Now()) {
		t.Fatalf("host row = %+v", h)
	}
	var order []string
	for _, x := range inv.Vms {
		order = append(order, x.Host+"/"+x.Name)
	}
	if strings.Join(order, " ") != "hv1/vm1 hv2/vm2 hv2/vm9" {
		t.Fatalf("order = %v", order)
	}
	g := inv.Vms[0]
	if g.Origin == nil || *g.Origin.Image != "img1" || g.Comment != "placeholder comment" || g.GoneSince != nil ||
		g.CredentialsKeptUntil != nil || len(g.Disks) != 1 || g.Disks[0].Kind != storeapi.File || g.Memory != 4 {
		t.Fatalf("vm row = %+v", g)
	}
	if inv.Vms[1].Origin != nil {
		t.Errorf("a VM with no origin reports one: %+v", inv.Vms[1].Origin)
	}
	if len(inv.Machines) != 1 || inv.Machines[0].Name != "m1" || inv.Machines[0].Kind != "metal" {
		t.Fatalf("machines = %+v", inv.Machines)
	}
	if only := e.inventory(t, "?host=hv2"); len(only.Hosts) != 1 || len(only.Vms) != 2 || only.Vms[0].Host != "hv2" {
		t.Fatalf("?host=hv2 = %+v", only)
	}
	if code, body := e.call(t, "GET", "/v1/inventory?host=a%20b", tokA, ""); code != 400 || !strings.Contains(body, "invalid field: host") {
		t.Fatalf("bad host = %d %s", code, body)
	}
	// No up/down or staleness field anywhere in the answer.
	_, raw := e.call(t, "GET", "/v1/inventory", tokA, "")
	for _, w := range []string{"\"up\"", "\"down\"", "\"online\"", "\"stale", "\"status\""} {
		if strings.Contains(raw, w) {
			t.Errorf("inventory carries %s", w)
		}
	}
	// User tokens only: a host token is 401 here, a bad token too.
	if code, _ := e.call(t, "GET", "/v1/inventory", tokH, ""); code != 401 {
		t.Errorf("host token on GET /v1/inventory = %d, want 401", code)
	}
	e.noSecretsInLogs(t)
}

func TestDelistAndDeleteRoutes(t *testing.T) {
	e := newInvEnv(t)
	e.report(t, "hv1", tokH, vm("vm1", invU1), vm("vm2", invU2))
	if code, _ := e.call(t, "PUT", "/v1/credentials/vms/hv1/vm1", tokA,
		`{"uuid":"`+invU1+`","username":"PLACEHOLDER-user","password":"PLACEHOLDER-not-a-secret-9"}`); code != 200 {
		t.Fatalf("put credential = %d", code)
	}

	code, body := e.call(t, "POST", "/v1/inventory/hv1/vm1/delist", tokA, "")
	var d storeapi.Delisted
	if code != 200 || json.Unmarshal([]byte(body), &d) != nil {
		t.Fatalf("delist = %d %s", code, body)
	}
	if d.Uuid != invU1 || !d.HadCredentials || !d.CredentialsKeptUntil.Equal(e.clock.Now().Add(db.GracePeriod)) {
		t.Fatalf("delisted = %+v", d)
	}
	if strings.Contains(body, "PLACEHOLDER-not-a-secret-9") {
		t.Fatal("delist answer carries a secret")
	}
	// No live row now: 404; also for an unknown VM.
	for _, p := range []string{"/v1/inventory/hv1/vm1/delist", "/v1/inventory/hv1/nope/delist"} {
		if code, _ := e.call(t, "POST", p, tokA, ""); code != 404 {
			t.Errorf("POST %s = %d, want 404", p, code)
		}
	}
	// vm2 has no credentials.
	_, body = e.call(t, "POST", "/v1/inventory/hv1/vm2/delist", tokA, "")
	if json.Unmarshal([]byte(body), &d) != nil || d.HadCredentials {
		t.Fatalf("vm2 delisted = %s", body)
	}
	// Hosts and VM names are validated.
	if code, _ := e.call(t, "POST", "/v1/inventory/hv.1/vm1/delist", tokA, ""); code != 400 {
		t.Errorf("bad host = %d, want 400", code)
	}

	// Delete: needs a uuid, removes at once, 404 after.
	if code, _ := e.call(t, "DELETE", "/v1/inventory/hv1/vm1", tokA, ""); code != 400 {
		t.Errorf("delete without uuid = %d, want 400", code)
	}
	if code, body := e.call(t, "DELETE", "/v1/inventory/hv1/vm1?uuid=nope", tokA, ""); code != 400 || !strings.Contains(body, "invalid field: uuid") {
		t.Errorf("delete bad uuid = %d %s", code, body)
	}
	if code, _ := e.call(t, "DELETE", "/v1/inventory/hv1/vm1?uuid="+invU2, tokA, ""); code != 404 {
		t.Errorf("delete with another VM's uuid = %d, want 404", code)
	}
	if code, _ := e.call(t, "DELETE", "/v1/inventory/hv1/vm1?uuid="+invU1, tokA, ""); code != 204 {
		t.Errorf("delete = %d, want 204", code)
	}
	if code, _ := e.call(t, "DELETE", "/v1/inventory/hv1/vm1?uuid="+invU1, tokA, ""); code != 404 {
		t.Errorf("second delete = %d, want 404", code)
	}
	inv := e.inventory(t, "?gone=true")
	if len(inv.Vms) != 1 || inv.Vms[0].Name != "vm2" {
		t.Fatalf("after delete: %+v", inv.Vms)
	}
	// The credential record is untouched by an inventory delete.
	if code, _ := e.call(t, "GET", "/v1/credentials/vms/hv1/vm1?uuid="+invU1, tokA, ""); code != 200 {
		t.Errorf("credential after inventory delete = %d, want 200", code)
	}
	// A host token reaches neither route.
	for _, r := range [][2]string{{"POST", "/v1/inventory/hv1/vm2/delist"}, {"DELETE", "/v1/inventory/hv1/vm2?uuid=" + invU2}} {
		if code, _ := e.call(t, r[0], r[1], tokH, ""); code != 401 {
			t.Errorf("%s %s with a host token = %d, want 401", r[0], r[1], code)
		}
	}
	e.noSecretsInLogs(t)
}

// The tranche's whole story over HTTP, on the injected clock.
func TestInventoryEndToEnd(t *testing.T) {
	e := newInvEnv(t)
	ctx := context.Background()
	sweep := func() db.SweepCounts {
		t.Helper()
		n, err := RunSweep(ctx, SweepDeps{DB: e.db, Now: e.clock.Now, Log: slog.New(slog.NewJSONHandler(e.logs, nil))})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	e.report(t, "hv1", tokH, vm("vm1", invU1), vm("vm2", invU2))
	if code, _ := e.call(t, "PUT", "/v1/credentials/vms/hv1/vm2", tokA,
		`{"uuid":"`+invU2+`","username":"PLACEHOLDER-user"}`); code != 200 {
		t.Fatalf("put credential = %d", code)
	}
	e.clock.Advance(time.Minute)
	reportAt := e.clock.Now()
	if r := e.report(t, "hv1", tokH, vm("vm1", invU1)); r.Listed != 1 || r.NewlyGone != 1 {
		t.Fatalf("second report = %+v", r)
	}

	if inv := e.inventory(t, ""); len(inv.Vms) != 1 || inv.Vms[0].Name != "vm1" {
		t.Fatalf("default list = %+v", inv.Vms)
	}
	inv := e.inventory(t, "?gone=true")
	if len(inv.Vms) != 2 || inv.Vms[0].GoneSince != nil || inv.Vms[1].GoneSince == nil {
		t.Fatalf("gone list = %+v", inv.Vms)
	}
	gone := inv.Vms[1]
	if !gone.GoneSince.Equal(reportAt) || gone.CredentialsKeptUntil == nil ||
		!gone.CredentialsKeptUntil.Equal(reportAt.Add(7*24*time.Hour)) || !gone.HasCredentials {
		t.Fatalf("gone row = %+v", gone)
	}
	if inv.Hosts[0].Live != 1 || inv.Hosts[0].Gone != 1 {
		t.Fatalf("host counts = %+v", inv.Hosts[0])
	}

	// Just short of 7 days: nothing goes.
	e.clock.Advance(7*24*time.Hour - time.Second)
	if n := sweep(); n.Rows != 0 || n.Credentials != 0 {
		t.Fatalf("early sweep = %+v", n)
	}
	e.clock.Advance(time.Second)
	if n := sweep(); n.Rows != 1 || n.Credentials != 1 {
		t.Fatalf("sweep at 7 days = %+v", n)
	}
	if inv := e.inventory(t, "?gone=true"); len(inv.Vms) != 1 || inv.Vms[0].Name != "vm1" {
		t.Fatalf("after sweep = %+v", inv.Vms)
	}
	if code, _ := e.call(t, "GET", "/v1/credentials/vms/hv1/vm2?uuid="+invU2, tokA, ""); code != 404 {
		t.Fatalf("swept credential = %d, want 404", code)
	}

	// A year with no report: the host's silence is not a reason to drop anything.
	e.clock.Advance(365 * 24 * time.Hour)
	if n := sweep(); n.Rows != 0 {
		t.Fatalf("sweep a year later = %+v", n)
	}
	inv = e.inventory(t, "")
	if len(inv.Vms) != 1 || inv.Vms[0].Name != "vm1" || inv.Vms[0].GoneSince != nil {
		t.Fatalf("live row after a year = %+v", inv.Vms)
	}
	e.noSecretsInLogs(t)
}

// ---- the schedule ----------------------------------------------------------

func TestSweepSchedule(t *testing.T) {
	d := openTestDB(t)
	clock := newInvClock()
	d.SetClock(clock.Now)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := d.ApplyReport(ctx, "hv1", clock.Now(), "2.6.0", []db.VMRow{{UUID: invU1, Name: "vm1", State: "running"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ApplyReport(ctx, "hv1", clock.Now().Add(time.Second), "2.6.0", nil); err != nil { // vm1 goes gone
		t.Fatal(err)
	}
	// After records the wait it was given and hands the test the tick.
	armed := make(chan time.Duration)
	tick := make(chan time.Time)
	after := func(w time.Duration) <-chan time.Time { armed <- w; return tick }

	// The sweep at start finds nothing due; then the clock moves 8 days.
	done := StartSweeps(ctx, SweepDeps{DB: d, Now: clock.Now, After: after, Log: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))})
	if w := <-armed; w != 24*time.Hour {
		t.Fatalf("interval = %v, want 24h", w)
	}
	if inv, _ := d.ListInventory(ctx, "", true); len(inv.VMs) != 1 {
		t.Fatalf("the start sweep removed a row that was not due: %+v", inv.VMs)
	}
	clock.Advance(8 * 24 * time.Hour)
	tick <- clock.Now()
	<-armed // the second sweep is done
	if inv, _ := d.ListInventory(ctx, "", true); len(inv.VMs) != 0 {
		t.Fatalf("the daily sweep left a due row: %+v", inv.VMs)
	}
	cancel()
	<-done
}

func TestSweepAtStartRemovesDueRows(t *testing.T) {
	d := openTestDB(t)
	clock := newInvClock()
	d.SetClock(clock.Now)
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = d.ApplyReport(ctx, "hv1", clock.Now(), "2.6.0", []db.VMRow{{UUID: invU1, Name: "vm1", State: "running"}})
	_, _ = d.ApplyReport(ctx, "hv1", clock.Now().Add(time.Second), "2.6.0", nil)
	clock.Advance(30 * 24 * time.Hour) // the store was down for a month
	armed := make(chan struct{})
	done := StartSweeps(ctx, SweepDeps{DB: d, Now: clock.Now, After: func(time.Duration) <-chan time.Time { armed <- struct{}{}; return nil }})
	<-armed
	if inv, _ := d.ListInventory(ctx, "", true); len(inv.VMs) != 0 {
		t.Fatalf("start sweep left a due row: %+v", inv.VMs)
	}
	cancel()
	<-done
}

// A failed sweep logs a class and nothing the database said.
func TestSweepFailureLogsClassOnly(t *testing.T) {
	d := openTestDB(t)
	_ = d.Close() // every query now fails with an error text of its own
	var logs bytes.Buffer
	_, err := RunSweep(context.Background(), SweepDeps{DB: d, Log: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err == nil {
		t.Fatal("sweep on a closed database succeeded")
	}
	out := logs.String()
	if !strings.Contains(out, "inventory sweep failed") || !strings.Contains(out, `"class":"sweep"`) {
		t.Fatalf("log = %s", out)
	}
	if strings.Contains(out, err.Error()) || strings.Contains(out, "closed") || strings.Contains(out, `"err"`) {
		t.Fatalf("log carries the error text: %s", out)
	}
}
