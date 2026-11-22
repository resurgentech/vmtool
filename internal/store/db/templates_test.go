package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func tpl(name, kind, pool string) TemplateRow {
	return TemplateRow{Name: name, Kind: kind, Pool: pool, SizeBytes: 10 << 30}
}

func listTpl(t *testing.T, d *DB, host string) Inventory {
	t.Helper()
	inv, err := d.ListInventory(context.Background(), host, false)
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func hostRow(t *testing.T, inv Inventory, name string) HostRow {
	t.Helper()
	for _, h := range inv.Hosts {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("no host %s in %+v", name, inv.Hosts)
	return HostRow{}
}

// A report with templates replaces the host's list and sets the flag; one
// without leaves both; an empty list clears the rows and keeps the flag.
func TestApplyReportTemplates(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)

	// A first report with no templates (an older daemon): flag false, no rows.
	if _, err := d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")}); err != nil {
		t.Fatal(err)
	}
	inv := listTpl(t, d, "")
	if hostRow(t, inv, "hv1").TemplatesReported || len(inv.Templates) != 0 {
		t.Fatalf("old-style first report: %+v", inv)
	}

	created := t0.Add(-time.Hour)
	img := TemplateRow{Name: "img1", Kind: "image", Pool: "pool1", SourceImage: "img1", SizeBytes: 20 << 30, Created: &created}
	saved := tpl("golden", "saved", "pool1")
	c.t = t0.Add(time.Minute)
	if _, err := d.ApplyReportWithTemplates(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")}, &[]TemplateRow{saved, img}); err != nil {
		t.Fatal(err)
	}
	inv = listTpl(t, d, "")
	if !hostRow(t, inv, "hv1").TemplatesReported || len(inv.Templates) != 2 {
		t.Fatalf("after templates: %+v", inv)
	}
	// Ordered by host, name, pool: golden before img1.
	g, i := inv.Templates[0], inv.Templates[1]
	if g.Host != "hv1" || g.Name != "golden" || g.Kind != "saved" || g.SourceImage != "" || g.Created != nil {
		t.Errorf("saved row %+v", g)
	}
	if i.Name != "img1" || i.Kind != "image" || i.SourceImage != "img1" || i.SizeBytes != 20<<30 || i.Created == nil || !i.Created.Equal(created) {
		t.Errorf("image row %+v", i)
	}

	// An old-style report afterwards keeps rows and flag.
	c.t = t0.Add(2 * time.Minute)
	if _, err := d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")}); err != nil {
		t.Fatal(err)
	}
	inv = listTpl(t, d, "")
	if !hostRow(t, inv, "hv1").TemplatesReported || len(inv.Templates) != 2 {
		t.Fatalf("after old-style report: %+v", inv)
	}

	// Another host is independent, and the host filter limits templates.
	c.t = t0.Add(3 * time.Minute)
	if _, err := d.ApplyReportWithTemplates(ctx, "hv2", t0, "v", nil, &[]TemplateRow{tpl("other", "saved", "p")}); err != nil {
		t.Fatal(err)
	}
	if inv = listTpl(t, d, "hv2"); len(inv.Templates) != 1 || inv.Templates[0].Host != "hv2" {
		t.Fatalf("host filter: %+v", inv.Templates)
	}
	if inv = listTpl(t, d, ""); len(inv.Templates) != 3 {
		t.Fatalf("both hosts: %+v", inv.Templates)
	}

	// An empty list clears hv1's rows, keeps the flag, leaves hv2 alone.
	c.t = t0.Add(4 * time.Minute)
	if _, err := d.ApplyReportWithTemplates(ctx, "hv1", t0, "v", nil, &[]TemplateRow{}); err != nil {
		t.Fatal(err)
	}
	inv = listTpl(t, d, "")
	if !hostRow(t, inv, "hv1").TemplatesReported || len(inv.Templates) != 1 || inv.Templates[0].Host != "hv2" {
		t.Fatalf("after empty list: %+v", inv)
	}
}

// Two templates of one name in one pool (a stale and a current image template)
// are both kept.
func TestApplyReportKeepsDuplicateNames(t *testing.T) {
	d, _ := invDB(t)
	a, b := tpl("img1", "image", "pool1"), tpl("img1", "image", "pool1")
	b.SizeBytes = 11 << 30
	if _, err := d.ApplyReportWithTemplates(context.Background(), "hv1", t0, "v", nil, &[]TemplateRow{a, b}); err != nil {
		t.Fatal(err)
	}
	if inv := listTpl(t, d, ""); len(inv.Templates) != 2 {
		t.Fatalf("%+v", inv.Templates)
	}
}

// A stale report (received before the last applied one) changes no template.
func TestStaleReportLeavesTemplates(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	c.t = t0.Add(time.Hour)
	if _, err := d.ApplyReportWithTemplates(ctx, "hv1", t0, "v", nil, &[]TemplateRow{tpl("a", "saved", "p")}); err != nil {
		t.Fatal(err)
	}
	c.t = t0
	cnt, err := d.ApplyReportWithTemplates(ctx, "hv1", t0, "v", nil, &[]TemplateRow{})
	if err != nil || !cnt.Stale {
		t.Fatalf("%+v %v", cnt, err)
	}
	if inv := listTpl(t, d, ""); len(inv.Templates) != 1 {
		t.Fatalf("stale report changed templates: %+v", inv.Templates)
	}
}

// Migration 9 adds the template table and the host flag to a version-8
// database; every old row survives and its host reports no templates.
func TestMigration9UpgradesVersion8Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d", "store.db")
	if len(migrations) < 9 {
		t.Fatalf("want at least 9 migrations, have %d", len(migrations))
	}
	d, err := openMigrations(ctx, path, key(1), migrations[:8])
	if err != nil {
		t.Fatal(err)
	}
	stamp := t0.Format(time.RFC3339Nano)
	for _, q := range []string{
		`INSERT INTO hosts(name, last_report, generated_at, daemon_version) VALUES ('hv1', '` + stamp + `', '` + stamp + `', '2.9.0')`,
		`INSERT INTO hosts(name, last_report, generated_at, daemon_version) VALUES ('hv2', '` + stamp + `', '` + stamp + `', '2.6.0')`,
		`INSERT INTO vms(host, uuid, name, state, vcpus, memory_gib, ip, autostart, firmware, disks, first_seen, last_seen, gone_since, disks_unreadable, networks)
		 VALUES ('hv1', '` + uuid1 + `', 'vm1', 'running', 2, 4, '', 0, '', '[]', '` + stamp + `', '` + stamp + `', NULL, 0, '[]')`,
		`INSERT INTO vms(host, uuid, name, state, vcpus, memory_gib, ip, autostart, firmware, disks, first_seen, last_seen, gone_since, disks_unreadable, networks)
		 VALUES ('hv2', '` + uuid2 + `', 'vm2', 'shutoff', 1, 1, '', 0, '', '[]', '` + stamp + `', '` + stamp + `', '` + stamp + `', 0, '[]')`,
		`INSERT INTO vm_notes(host, uuid, name, comment, comment_updated) VALUES ('hv1', '` + uuid1 + `', 'vm1', 'keep me', '` + stamp + `')`,
		`INSERT INTO images(name, sha256, size, updated) VALUES ('img1', '` + hash + `', 5, '` + stamp + `')`,
		`INSERT INTO machines(name, kind, address, credentials, updated) VALUES ('nuc1', 'metal', '192.0.2.5', x'00', '` + stamp + `')`,
		`INSERT INTO networks(name, vni, subnet, domain, home, created) VALUES ('net1', 100, '10.77.1.0/24', 'net1.test', 'hv1', '` + stamp + `')`,
		`INSERT INTO network_members(network, host, lan_ip, lan_dev, joined) VALUES ('net1', 'hv1', '192.0.2.1', 'eth0', '` + stamp + `')`,
		`INSERT INTO network_reservations(network, mac, ip, host, vm, created, uuid) VALUES ('net1', '52:54:00:00:00:01', '10.77.1.100', 'hv1', 'vm1', '` + stamp + `', '` + uuid1 + `')`,
	} {
		if _, err := d.sql.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close()

	d, err = Open(ctx, path, key(1))
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer d.Close()
	if v, _ := d.version(ctx); v != len(migrations) {
		t.Fatalf("version %d want %d", v, len(migrations))
	}
	for table, want := range map[string]int{
		"hosts": 2, "vms": 2, "vm_notes": 1, "images": 1, "machines": 1, "networks": 1, "network_members": 1, "network_reservations": 1,
	} {
		var n int
		if err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != want {
			t.Errorf("%s: %d rows (err %v), want %d", table, n, err, want)
		}
	}
	inv, err := d.ListInventory(ctx, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Hosts) != 2 || len(inv.VMs) != 2 || len(inv.Templates) != 0 {
		t.Fatalf("after upgrade: %+v", inv)
	}
	for _, h := range inv.Hosts {
		if h.TemplatesReported {
			t.Errorf("host %s claims templates_reported after the upgrade", h.Name)
		}
	}
	if v, ok := vmByUUID(t, d, "hv1", uuid1, false); !ok || v.Comment != "keep me" {
		t.Errorf("vm1 after upgrade %+v ok=%v", v, ok)
	}
	// The new tables work on the upgraded database.
	if _, err := d.ApplyReportWithTemplates(ctx, "hv1", t0, "v", nil, &[]TemplateRow{tpl("a", "saved", "p")}); err != nil {
		t.Fatal(err)
	}
	if inv, _ = d.ListInventory(ctx, "hv1", false); len(inv.Templates) != 1 || !hostRow(t, inv, "hv1").TemplatesReported {
		t.Fatalf("after first template report: %+v", inv)
	}
}
