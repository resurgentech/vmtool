package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/storeapi"
)

// Templates in the host report and in the inventory. Placeholders only:
// hosts hv1 and hv2, images img1, template golden, pool pool1.

func (e *invEnv) reportTemplates(t *testing.T, host, tok string, tpls *[]storeapi.ReportTemplate, vms ...storeapi.ReportVM) {
	t.Helper()
	if vms == nil {
		vms = []storeapi.ReportVM{}
	}
	b, err := json.Marshal(storeapi.HostReport{GeneratedAt: e.clock.Now(), DaemonVersion: "2.10.0", Vms: vms, Templates: tpls})
	if err != nil {
		t.Fatal(err)
	}
	if code, body := e.call(t, "PUT", "/v1/hosts/"+host+"/inventory", tok, string(b)); code != 200 {
		t.Fatalf("report = %d %s", code, body)
	}
}

func hostNamed(t *testing.T, inv storeapi.Inventory, name string) storeapi.InventoryHost {
	t.Helper()
	for _, h := range inv.Hosts {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("no host %s: %+v", name, inv.Hosts)
	return storeapi.InventoryHost{}
}

func TestReportTemplatesRoundTrip(t *testing.T) {
	e := newInvEnv(t)
	created := e.clock.Now().Add(-48 * time.Hour)
	src := "img1"
	tpls := []storeapi.ReportTemplate{
		{Name: "img1", Kind: storeapi.ReportTemplateKindImage, Pool: "pool1", SourceImage: &src, SizeBytes: 20 << 30, Created: &created},
		{Name: "golden", Kind: storeapi.ReportTemplateKindSaved, Pool: "pool1", SizeBytes: 10 << 30},
	}
	e.reportTemplates(t, "hv1", tokH, &tpls, vm("vm1", invU1))

	inv := e.inventory(t, "")
	if !hostNamed(t, inv, "hv1").TemplatesReported {
		t.Fatalf("templates_reported is false: %+v", inv.Hosts)
	}
	if len(inv.Templates) != 2 {
		t.Fatalf("templates %+v", inv.Templates)
	}
	g, i := inv.Templates[0], inv.Templates[1] // by name: golden, img1
	if g.Host != "hv1" || g.Name != "golden" || g.Kind != storeapi.InventoryTemplateKindSaved || g.Pool != "pool1" ||
		g.SizeBytes != 10<<30 || g.SourceImage != nil || g.Created != nil {
		t.Errorf("saved %+v", g)
	}
	if i.Host != "hv1" || i.Name != "img1" || i.Kind != storeapi.InventoryTemplateKindImage || i.SourceImage == nil || *i.SourceImage != "img1" ||
		i.SizeBytes != 20<<30 || i.Created == nil || !i.Created.Equal(created) {
		t.Errorf("image %+v", i)
	}
	// The host filter limits templates too.
	e.reportTemplates(t, "hv2", tokH2, &[]storeapi.ReportTemplate{{Name: "other", Kind: storeapi.ReportTemplateKindSaved, Pool: "p", SizeBytes: 1}})
	if inv = e.inventory(t, "?host=hv2"); len(inv.Templates) != 1 || inv.Templates[0].Host != "hv2" {
		t.Errorf("host=hv2: %+v", inv.Templates)
	}
	if inv = e.inventory(t, ""); len(inv.Templates) != 3 {
		t.Errorf("all: %+v", inv.Templates)
	}
	// The list is replaced whole by the next one.
	e.reportTemplates(t, "hv1", tokH, &[]storeapi.ReportTemplate{{Name: "golden", Kind: storeapi.ReportTemplateKindSaved, Pool: "pool1", SizeBytes: 5}}, vm("vm1", invU1))
	if inv = e.inventory(t, "?host=hv1"); len(inv.Templates) != 1 || inv.Templates[0].SizeBytes != 5 {
		t.Errorf("replaced: %+v", inv.Templates)
	}
}

// A report with no templates key, from an older daemon, leaves
// templates_reported false for a host that never sent any, and changes no
// stored row of one that did.
func TestReportWithoutTemplatesFromAnOldDaemon(t *testing.T) {
	e := newInvEnv(t)
	e.report(t, "hv1", tokH, vm("vm1", invU1)) // reportBody has no templates key
	inv := e.inventory(t, "")
	if hostNamed(t, inv, "hv1").TemplatesReported || len(inv.Templates) != 0 || inv.Templates == nil {
		t.Fatalf("old daemon, first report: reported=%v templates=%#v", hostNamed(t, inv, "hv1").TemplatesReported, inv.Templates)
	}
	if code, body := e.call(t, "GET", "/v1/inventory", tokA, ""); code != 200 || !strings.Contains(body, `"templates":[]`) {
		t.Fatalf("templates not an empty list: %d %s", code, body)
	}

	// Once the host has sent templates, an old-style report keeps them.
	e.clock.Advance(time.Minute)
	e.reportTemplates(t, "hv1", tokH, &[]storeapi.ReportTemplate{{Name: "golden", Kind: storeapi.ReportTemplateKindSaved, Pool: "pool1", SizeBytes: 1}}, vm("vm1", invU1))
	e.clock.Advance(time.Minute)
	e.report(t, "hv1", tokH, vm("vm1", invU1))
	inv = e.inventory(t, "")
	if !hostNamed(t, inv, "hv1").TemplatesReported || len(inv.Templates) != 1 {
		t.Fatalf("after an old-style report: %+v %+v", inv.Hosts, inv.Templates)
	}
}

// An empty list is a report of no templates: the flag is set.
func TestReportEmptyTemplateList(t *testing.T) {
	e := newInvEnv(t)
	e.reportTemplates(t, "hv1", tokH, &[]storeapi.ReportTemplate{})
	inv := e.inventory(t, "")
	if !hostNamed(t, inv, "hv1").TemplatesReported || len(inv.Templates) != 0 {
		t.Fatalf("%+v %+v", inv.Hosts, inv.Templates)
	}
}

// A host that stops reporting keeps its templates: nothing is removed by age
// or silence, nor when the VMs are all gone, nor with gone=true or not.
func TestSleepingHostKeepsItsTemplates(t *testing.T) {
	e := newInvEnv(t)
	e.reportTemplates(t, "hv1", tokH, &[]storeapi.ReportTemplate{{Name: "golden", Kind: storeapi.ReportTemplateKindSaved, Pool: "pool1", SizeBytes: 1}}, vm("vm1", invU1))
	e.clock.Advance(90 * 24 * time.Hour)
	if _, err := RunSweep(t.Context(), SweepDeps{DB: e.db, Now: e.clock.Now}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "?gone=true", "?host=hv1"} {
		inv := e.inventory(t, q)
		if len(inv.Templates) != 1 || inv.Templates[0].Name != "golden" || !hostNamed(t, inv, "hv1").TemplatesReported {
			t.Errorf("inventory%s after 90 days of silence: %+v", q, inv.Templates)
		}
	}
}

func TestReportTemplateValidation(t *testing.T) {
	e := newInvEnv(t)
	long := strings.Repeat("x", 600)
	many := make([]storeapi.ReportTemplate, 1025)
	for i := range many {
		many[i] = storeapi.ReportTemplate{Name: "t", Kind: storeapi.ReportTemplateKindSaved, Pool: "p", SizeBytes: 1}
	}
	cases := map[string]storeapi.ReportTemplate{
		"kind":         {Name: "t", Kind: "clone", Pool: "p", SizeBytes: 1},
		"name":         {Name: "", Kind: storeapi.ReportTemplateKindSaved, Pool: "p", SizeBytes: 1},
		"name (long)":  {Name: long, Kind: storeapi.ReportTemplateKindSaved, Pool: "p", SizeBytes: 1},
		"pool":         {Name: "t", Kind: storeapi.ReportTemplateKindSaved, Pool: "p\nq", SizeBytes: 1},
		"size_bytes":   {Name: "t", Kind: storeapi.ReportTemplateKindSaved, Pool: "p", SizeBytes: -1},
		"source_image": {Name: "t", Kind: storeapi.ReportTemplateKindImage, Pool: "p", SizeBytes: 1, SourceImage: &long},
	}
	for want, tp := range cases {
		b, _ := json.Marshal(storeapi.HostReport{GeneratedAt: e.clock.Now(), DaemonVersion: "v", Vms: []storeapi.ReportVM{}, Templates: &[]storeapi.ReportTemplate{tp}})
		code, body := e.call(t, "PUT", "/v1/hosts/hv1/inventory", tokH, string(b))
		if code != 400 || !strings.Contains(body, "templates[0]") || !strings.Contains(body, strings.Fields(want)[0]) {
			t.Errorf("%s: %d %s", want, code, body)
		}
	}
	b, _ := json.Marshal(storeapi.HostReport{GeneratedAt: e.clock.Now(), DaemonVersion: "v", Vms: []storeapi.ReportVM{}, Templates: &many})
	if code, body := e.call(t, "PUT", "/v1/hosts/hv1/inventory", tokH, string(b)); code != 400 || !strings.Contains(body, "templates") {
		t.Errorf("too many: %d %s", code, body)
	}
	if inv := e.inventory(t, ""); len(inv.Hosts) != 0 || len(inv.Templates) != 0 {
		t.Errorf("a refused report left something: %+v", inv)
	}
}
