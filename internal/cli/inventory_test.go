package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
)

// N9: vmtool inventory prints the store's hosts, VMs and machines.

var invKeep = map[string]bool{"vmtool inventory": true}

func inventoryBody() map[string]any {
	return map[string]any{
		"hosts": []any{map[string]any{"name": "hv1", "last_report": "2026-11-21T01:02:03Z", "daemon_version": "2.6.0", "live": 1, "gone": 1, "templates_reported": true}},
		"templates": []any{
			map[string]any{"host": "hv1", "name": "golden", "kind": "saved", "pool": "pool1", "size_bytes": 10737418240},
			map[string]any{"host": "hv1", "name": "img1", "kind": "image", "pool": "pool1", "source_image": "img1", "size_bytes": 21474836480, "created": "2026-11-19T00:00:00Z"},
		},
		"vms": []any{
			map[string]any{"host": "hv1", "name": "vm1", "uuid": "00000000-0000-4000-8000-000000000001", "state": "running",
				"vcpus": 2, "memory": 4, "ip": "10.0.0.5", "autostart": true, "firmware": "efi",
				"disks": []any{
					map[string]any{"target": "vda", "pool": "nvme", "capacity_bytes": 21474836480, "kind": "thin"},
					map[string]any{"target": "vdb", "pool": "nvme", "capacity_bytes": 1073741824, "kind": "file"}},
				"origin":  map[string]any{"image": "img1", "image_sha256": strings.Repeat("a", 64), "clone": "thin", "pool": "nvme", "created": "2026-11-20T00:00:00Z"},
				"comment": "placeholder comment", "first_seen": "2026-11-20T00:00:00Z", "last_seen": "2026-11-21T01:02:03Z", "has_credentials": true},
			map[string]any{"host": "hv1", "name": "vm2", "uuid": "00000000-0000-4000-8000-000000000002", "state": "shutoff",
				"vcpus": 1, "memory": 1, "ip": "", "autostart": false, "firmware": "bios", "disks": []any{}, "comment": "",
				"first_seen": "2026-11-20T00:00:00Z", "last_seen": "2026-11-20T12:00:00Z", "gone_since": "2026-11-21T01:02:03Z",
				"credentials_kept_until": "2026-11-28T01:02:03Z", "has_credentials": false},
		},
		"machines": []any{map[string]any{"name": "nuc1", "kind": "metal"}},
	}
}

func TestInventoryTables(t *testing.T) {
	s := newCLIStore(t)
	s.inv = inventoryBody()
	dir := storeWorkdir(t, s, storeTok)
	r := runGate(t, dir, invKeep, "inventory")
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.errOut)
	}
	if q, _ := s.invQuery.Load().(string); q != "/v1/inventory" {
		t.Errorf("store saw %q", q)
	}
	for _, want := range []string{
		"hv1", "2026-11-21T01:02:03Z", "2.6.0", // host row: name, plain last report, version
		"hv1/vm1", "running", "10.0.0.5", "vda 20G thin", "vdb 1G file", "img1 (thin)", "2026-11-20T00:00:00Z", "placeholder comment",
		"nuc1", "metal",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q:\n%s", want, r.out)
		}
	}
	// A VM its host no longer has appears only with --gone; no up/down or age words.
	if strings.Contains(r.out, "vm2") {
		t.Errorf("gone VM listed without --gone:\n%s", r.out)
	}
	for _, bad := range []string{" ago", "stale", "STALE", "offline", "unreachable", " down", " up "} {
		if strings.Contains(r.out, bad) {
			t.Errorf("output has %q:\n%s", bad, r.out)
		}
	}
	for _, sec := range []string{"HOSTS", "VMS", "MACHINES"} {
		if !strings.Contains(r.out, sec) {
			t.Errorf("no %s section:\n%s", sec, r.out)
		}
	}
	if strings.Index(r.out, "HOSTS") > strings.Index(r.out, "VMS") || strings.Index(r.out, "VMS") > strings.Index(r.out, "MACHINES") {
		t.Errorf("sections out of order:\n%s", r.out)
	}
	if strings.Contains(r.out, storeTok) || strings.Contains(r.errOut, storeTok) {
		t.Error("the store token is printed")
	}
}

// The templates are listed between the VMs and the machines, each with its
// host, kind, pool, source image, size and creation time; a field the host did
// not report is a dash. The host row says how many it reported.
func TestInventoryListsTemplates(t *testing.T) {
	s := newCLIStore(t)
	s.inv = inventoryBody()
	dir := storeWorkdir(t, s, storeTok)
	r := runGate(t, dir, invKeep, "inventory")
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.errOut)
	}
	if !strings.Contains(r.out, "TEMPLATES") {
		t.Fatalf("no TEMPLATES section:\n%s", r.out)
	}
	if strings.Index(r.out, "\nVMS") > strings.Index(r.out, "\nTEMPLATES") || strings.Index(r.out, "\nTEMPLATES") > strings.Index(r.out, "\nMACHINES") {
		t.Errorf("TEMPLATES is not between VMS and MACHINES:\n%s", r.out)
	}
	var golden, img string
	for _, ln := range strings.Split(r.out, "\n") {
		switch {
		case strings.HasPrefix(ln, "hv1/golden"):
			golden = ln
		case strings.HasPrefix(ln, "hv1/img1"):
			img = ln
		}
	}
	if f := strings.Fields(golden); len(f) != 6 || f[1] != "saved" || f[2] != "pool1" || f[3] != "-" || f[4] != "10G" || f[5] != "-" {
		t.Errorf("saved template row %q", golden)
	}
	if f := strings.Fields(img); len(f) != 6 || f[1] != "image" || f[2] != "pool1" || f[3] != "img1" || f[4] != "20G" || f[5] != "2026-11-19T00:00:00Z" {
		t.Errorf("image template row %q", img)
	}
}

// A host whose daemon never sent templates says so, so the lack of rows is
// not read as "none"; one that did and has none says 0.
func TestInventoryTemplatesNotReported(t *testing.T) {
	body := inventoryBody()
	body["templates"] = []any{}
	body["hosts"] = []any{
		map[string]any{"name": "hv1", "last_report": "2026-11-21T01:02:03Z", "daemon_version": "2.9.0", "live": 1, "gone": 0},
		map[string]any{"name": "hv2", "last_report": "2026-11-21T01:02:03Z", "daemon_version": "2.10.0", "live": 0, "gone": 0, "templates_reported": true},
	}
	s := newCLIStore(t)
	s.inv = body
	dir := storeWorkdir(t, s, storeTok)
	r := runGate(t, dir, invKeep, "inventory")
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.errOut)
	}
	var h1, h2 string
	for _, ln := range strings.Split(r.out, "\n") {
		switch {
		case strings.HasPrefix(ln, "hv1 "):
			h1 = ln
		case strings.HasPrefix(ln, "hv2 "):
			h2 = ln
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(h1), "not reported") || !strings.HasSuffix(strings.TrimSpace(h2), "0") {
		t.Errorf("host rows:\n%q\n%q\n%s", h1, h2, r.out)
	}
	if !strings.Contains(r.out, "TEMPLATES\n") || !strings.Contains(r.out, "(none)") {
		t.Errorf("no empty TEMPLATES section:\n%s", r.out)
	}
}

const unreadableMark = "disks unreadable on the host; last known shown"

// A VM whose disks the host could not read is marked, with the last known
// disks still shown; without one there is no mark, in tables or JSON.
func TestInventoryMarksUnreadableDisks(t *testing.T) {
	s := newCLIStore(t)
	s.inv = inventoryBody()
	dir := storeWorkdir(t, s, storeTok)
	r := runGate(t, dir, invKeep, "inventory")
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.errOut)
	}
	if strings.Contains(r.out, "unreadable") {
		t.Errorf("a mark with nothing to mark:\n%s", r.out)
	}

	body := inventoryBody()
	body["vms"].([]any)[0].(map[string]any)["disks_unreadable"] = true
	s = newCLIStore(t)
	s.inv = body
	dir = storeWorkdir(t, s, storeTok)
	r = runGate(t, dir, invKeep, "inventory")
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.errOut)
	}
	if !strings.Contains(r.out, unreadableMark) || !strings.Contains(r.out, "vda 20G thin") {
		t.Errorf("no mark, or the last known disks are gone:\n%s", r.out)
	}
	r = runGate(t, dir, invKeep, "inventory", "--json")
	var inv app.Inventory
	if err := json.Unmarshal([]byte(r.out), &inv); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.out)
	}
	if inv.Vms[0].DisksUnreadable == nil || !*inv.Vms[0].DisksUnreadable || !strings.Contains(r.out, `"disks_unreadable": true`) && !strings.Contains(r.out, `"disks_unreadable":true`) {
		t.Errorf("flag not in --json: %s", r.out)
	}
}

func TestInventoryGoneAndHost(t *testing.T) {
	s := newCLIStore(t)
	s.inv = inventoryBody()
	dir := storeWorkdir(t, s, storeTok)
	r := runGate(t, dir, invKeep, "inventory", "--gone", "--host", "hv1")
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.errOut)
	}
	if q, _ := s.invQuery.Load().(string); q != "/v1/inventory?gone=true&host=hv1" {
		t.Errorf("store saw %q", q)
	}
	for _, want := range []string{"hv1/vm2", "shutoff", "2026-11-21T01:02:03Z", "2026-11-28T01:02:03Z"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("output lacks %q:\n%s", want, r.out)
		}
	}
}

func TestInventoryJSON(t *testing.T) {
	s := newCLIStore(t)
	s.inv = inventoryBody()
	dir := storeWorkdir(t, s, storeTok)
	r := runGate(t, dir, invKeep, "inventory", "--json", "--gone")
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.errOut)
	}
	var inv app.Inventory
	if err := json.Unmarshal([]byte(r.out), &inv); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, r.out)
	}
	if len(inv.Hosts) != 1 || len(inv.Vms) != 2 || len(inv.Machines) != 1 || inv.Vms[1].GoneSince == nil || inv.Vms[0].Origin == nil {
		t.Errorf("decoded %+v", inv)
	}
	if !inv.Hosts[0].TemplatesReported || len(inv.Templates) != 2 || inv.Templates[1].Name != "img1" ||
		inv.Templates[1].SourceImage == nil || *inv.Templates[1].SourceImage != "img1" {
		t.Errorf("templates missing from --json: %+v", inv.Templates)
	}
	if strings.Contains(r.out, "HOSTS") {
		t.Errorf("tables mixed into --json:\n%s", r.out)
	}
}

// No store configured, or one that does not answer: one clear message, an
// error (exit 1), nothing on stdout, and no "using local" line besides it.
func TestInventoryNeedsAStore(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, "name: ws\ntoken_sha256: "+config.Hash(gateToken)+"\n", "")
	r := runGate(t, dir, invKeep, "inventory")
	if r.err == nil || !strings.Contains(r.err.Error(), "no store is configured") || r.out != "" {
		t.Fatalf("no store: %v, out %q", r.err, r.out)
	}

	s := newCLIStore(t)
	s.inv = inventoryBody()
	s.down.Store(true)
	dir = storeWorkdir(t, s, storeTok)
	r = runGate(t, dir, invKeep, "inventory")
	if r.err == nil || !strings.Contains(r.err.Error(), "is not reachable") || r.out != "" {
		t.Fatalf("store down: %v, out %q", r.err, r.out)
	}
	if strings.Contains(r.errOut, "using this machine") || strings.Contains(r.err.Error(), storeTok) || strings.Contains(r.errOut, storeTok) {
		t.Errorf("fallback line or token in %q / %v", r.errOut, r.err)
	}
}
