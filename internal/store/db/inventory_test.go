package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

const (
	uuid1 = "00000000-0000-4000-8000-000000000001"
	uuid2 = "00000000-0000-4000-8000-000000000002"
	uuid3 = "00000000-0000-4000-8000-000000000003"
	hash  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func invDB(t *testing.T) (*DB, *clock) {
	t.Helper()
	d, _ := openTemp(t)
	c := &clock{t: t0}
	d.SetClock(c.now)
	return d, c
}

func str(s string) *string { return &s }

func row(uuid, name string) VMRow {
	return VMRow{UUID: uuid, Name: name, State: "running", VCPUs: 2, MemoryGiB: 4, IP: "192.0.2.10",
		Autostart: true, Firmware: "efi"}
}

func vmByUUID(t *testing.T, d *DB, host, uuid string, withGone bool) (InventoryVM, bool) {
	t.Helper()
	inv, err := d.ListInventory(context.Background(), host, withGone)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range inv.VMs {
		if v.UUID == uuid {
			return v, true
		}
	}
	return InventoryVM{}, false
}

func TestMigration2UpgradesVersion1Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d", "store.db")
	d, err := openMigrations(ctx, path, key(1), migrations[:1])
	if err != nil {
		t.Fatal(err)
	}
	if err := d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "vm1", UUID: uuid1, Password: phPassword}); err != nil {
		t.Fatal(err)
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
	if _, err := d.GetVMCredential(ctx, "hv1", "vm1"); err != nil {
		t.Fatalf("credential lost: %v", err)
	}
	if _, err := d.ApplyReport(ctx, "hv1", t0, "2.6.0", []VMRow{row(uuid1, "vm1")}); err != nil {
		t.Fatal(err)
	}
	v, ok := vmByUUID(t, d, "hv1", uuid1, false)
	if !ok || !v.HasCredentials {
		t.Fatalf("row %+v ok=%v", v, ok)
	}
}

func TestApplyReportInsertUpdateAndGone(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	cnt, err := d.ApplyReport(ctx, "hv1", t0, "2.6.0", []VMRow{row(uuid1, "vm1"), row(uuid2, "vm2")})
	if err != nil {
		t.Fatal(err)
	}
	if cnt.Listed != 2 || cnt.NewlyGone != 0 || cnt.Relisted != 0 || cnt.Stale {
		t.Fatalf("counts %+v", cnt)
	}
	first := c.t
	c.t = t0.Add(time.Hour)
	r1 := row(uuid1, "vm1")
	r1.State = "shutoff"
	r1.Disks = []byte(`[{"target":"vda","pool":"p","capacity_bytes":1,"kind":"file"}]`)
	cnt, err = d.ApplyReport(ctx, "hv1", t0.Add(time.Hour), "2.6.1", []VMRow{r1})
	if err != nil {
		t.Fatal(err)
	}
	if cnt.Listed != 1 || cnt.NewlyGone != 1 || cnt.Relisted != 0 {
		t.Fatalf("counts %+v", cnt)
	}
	v, _ := vmByUUID(t, d, "hv1", uuid1, false)
	if v.State != "shutoff" || !v.FirstSeen.Equal(first) || !v.LastSeen.Equal(c.t) || v.GoneSince != nil {
		t.Fatalf("vm1 %+v", v)
	}
	if string(v.Disks) != `[{"target":"vda","pool":"p","capacity_bytes":1,"kind":"file"}]` {
		t.Fatalf("disks %s", v.Disks)
	}
	if _, ok := vmByUUID(t, d, "hv1", uuid2, false); ok {
		t.Fatal("gone row visible in default listing")
	}
	g, ok := vmByUUID(t, d, "hv1", uuid2, true)
	if !ok || g.GoneSince == nil || !g.GoneSince.Equal(c.t) {
		t.Fatalf("gone row %+v ok=%v", g, ok)
	}
	inv, _ := d.ListInventory(ctx, "", false)
	if len(inv.Hosts) != 1 {
		t.Fatalf("hosts %+v", inv.Hosts)
	}
	h := inv.Hosts[0]
	if h.Name != "hv1" || h.DaemonVersion != "2.6.1" || !h.LastReport.Equal(c.t) || h.Live != 1 || h.Gone != 1 {
		t.Fatalf("host %+v", h)
	}
	// A second report does not move gone_since of an already gone row.
	c.t = t0.Add(2 * time.Hour)
	cnt, _ = d.ApplyReport(ctx, "hv1", t0.Add(2*time.Hour), "2.6.1", []VMRow{r1})
	if cnt.NewlyGone != 0 {
		t.Fatalf("counts %+v", cnt)
	}
	g, _ = vmByUUID(t, d, "hv1", uuid2, true)
	if !g.GoneSince.Equal(t0.Add(time.Hour)) {
		t.Fatalf("gone_since moved: %v", g.GoneSince)
	}
}

func TestApplyReportRelist(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	c.t = t0.Add(time.Hour)
	d.ApplyReport(ctx, "hv1", c.t, "v", nil)
	c.t = t0.Add(2 * time.Hour)
	cnt, err := d.ApplyReport(ctx, "hv1", c.t, "v", []VMRow{row(uuid1, "vm1")})
	if err != nil {
		t.Fatal(err)
	}
	if cnt.Listed != 1 || cnt.Relisted != 1 {
		t.Fatalf("counts %+v", cnt)
	}
	v, ok := vmByUUID(t, d, "hv1", uuid1, false)
	if !ok || v.GoneSince != nil {
		t.Fatalf("not relisted: %+v", v)
	}
	if !v.FirstSeen.Equal(t0) {
		t.Fatalf("first_seen changed: %v", v.FirstSeen)
	}
}

func TestApplyReportStaleIgnored(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	c.t = t0.Add(time.Hour)
	d.ApplyReport(ctx, "hv1", t0, "new", []VMRow{row(uuid1, "vm1")})
	// The store's clock (the receive time) is what orders reports: this one
	// is received before the one already applied, whatever it says.
	c.t = t0
	cnt, err := d.ApplyReport(ctx, "hv1", t0.Add(24*time.Hour), "old", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cnt.Stale || cnt.Listed != 0 || cnt.NewlyGone != 0 {
		t.Fatalf("counts %+v", cnt)
	}
	if _, ok := vmByUUID(t, d, "hv1", uuid1, false); !ok {
		t.Fatal("stale report changed rows")
	}
	inv, _ := d.ListInventory(ctx, "hv1", false)
	if inv.Hosts[0].DaemonVersion != "new" || !inv.Hosts[0].LastReport.Equal(t0.Add(time.Hour)) {
		t.Fatalf("host %+v", inv.Hosts[0])
	}
	// The same receive time applies.
	c.t = t0.Add(time.Hour)
	cnt, _ = d.ApplyReport(ctx, "hv1", t0, "same", nil)
	if cnt.Stale || cnt.NewlyGone != 1 {
		t.Fatalf("counts %+v", cnt)
	}
}

func TestApplyReportOrderedByReceiveTimeNotGeneratedAt(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	year := 365 * 24 * time.Hour
	// A host with its clock a year ahead, then the same host honest.
	cnt, err := d.ApplyReport(ctx, "hv1", t0.Add(year), "ahead", []VMRow{row(uuid1, "vm1")})
	if err != nil || cnt.Stale || cnt.Listed != 1 {
		t.Fatalf("ahead: %+v %v", cnt, err)
	}
	c.t = t0.Add(time.Minute)
	cnt, err = d.ApplyReport(ctx, "hv1", t0.Add(time.Minute), "honest", []VMRow{row(uuid1, "vm1"), row(uuid2, "vm2")})
	if err != nil || cnt.Stale || cnt.Listed != 2 {
		t.Fatalf("honest after ahead: %+v %v", cnt, err)
	}
	if _, ok := vmByUUID(t, d, "hv1", uuid2, false); !ok {
		t.Fatal("the honest report was not applied")
	}
	// A report dated a year behind is applied too.
	c.t = t0.Add(2 * time.Minute)
	cnt, err = d.ApplyReport(ctx, "hv1", t0.Add(-year), "behind", []VMRow{row(uuid1, "vm1")})
	if err != nil || cnt.Stale || cnt.Listed != 1 || cnt.NewlyGone != 1 {
		t.Fatalf("behind: %+v %v", cnt, err)
	}
	// generated_at is still stored and shown.
	inv, _ := d.ListInventory(ctx, "hv1", false)
	h := inv.Hosts[0]
	if h.DaemonVersion != "behind" || !h.GeneratedAt.Equal(t0.Add(-year)) || !h.LastReport.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("host %+v", h)
	}
}

func TestApplyReportDisksUnreadableKeepsLastDisks(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	r1 := row(uuid1, "vm1")
	r1.Disks = []byte(`[{"target":"vda","pool":"p","capacity_bytes":1,"kind":"file"}]`)
	if _, err := d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{r1, row(uuid2, "vm2")}); err != nil {
		t.Fatal(err)
	}
	v, _ := vmByUUID(t, d, "hv1", uuid1, false)
	if v.DisksUnreadable {
		t.Fatal("flag set on a readable VM")
	}
	// vm1 is reported unreadable, with new fields but no disks; vm2 is a new
	// VM that was never read.
	c.t = t0.Add(time.Hour)
	u := row(uuid1, "vm1")
	u.State = "shutoff"
	u.DisksUnreadable = true
	u3 := row(uuid3, "vm3")
	u3.DisksUnreadable = true
	cnt, err := d.ApplyReport(ctx, "hv1", c.t, "v", []VMRow{u, row(uuid2, "vm2"), u3})
	if err != nil || cnt.Listed != 3 || cnt.NewlyGone != 0 {
		t.Fatalf("counts %+v %v", cnt, err)
	}
	v, _ = vmByUUID(t, d, "hv1", uuid1, false)
	if !v.DisksUnreadable || v.State != "shutoff" || string(v.Disks) != string(r1.Disks) {
		t.Fatalf("vm1 %+v disks %s", v, v.Disks)
	}
	v, _ = vmByUUID(t, d, "hv1", uuid3, false)
	if !v.DisksUnreadable || string(v.Disks) != "[]" {
		t.Fatalf("vm3 %+v disks %s", v, v.Disks)
	}
	// The next report that reads them clears the flag and replaces the disks.
	c.t = t0.Add(2 * time.Hour)
	r1.Disks = []byte(`[{"target":"vda","pool":"p","capacity_bytes":2,"kind":"file"}]`)
	if _, err := d.ApplyReport(ctx, "hv1", c.t, "v", []VMRow{r1}); err != nil {
		t.Fatal(err)
	}
	v, _ = vmByUUID(t, d, "hv1", uuid1, false)
	if v.DisksUnreadable || string(v.Disks) != string(r1.Disks) {
		t.Fatalf("vm1 after a readable report %+v disks %s", v, v.Disks)
	}
}

func TestMigration3UpgradesVersion2Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d", "store.db")
	d, err := openMigrations(ctx, path, key(1), migrations[:2])
	if err != nil {
		t.Fatal(err)
	}
	// A version-2 row, written with the version-2 columns only.
	stamp := t0.Format(time.RFC3339Nano)
	for _, q := range []string{
		`INSERT INTO hosts(name, last_report, generated_at, daemon_version) VALUES ('hv1', '` + stamp + `', '` + stamp + `', '2.6.0')`,
		`INSERT INTO vms(host, uuid, name, state, vcpus, memory_gib, ip, autostart, firmware, disks,
		   origin_image, origin_sha256, origin_template, origin_clone, origin_pool, created, comment, first_seen, last_seen, gone_since)
		 VALUES ('hv1', '` + uuid1 + `', 'vm1', 'running', 2, 4, '', 0, '', '[{"target":"vda","pool":"p","capacity_bytes":1,"kind":"file"}]',
		   '', '', '', '', '', '', '', '` + stamp + `', '` + stamp + `', NULL)`,
	} {
		if _, err := d.sql.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	d, err = Open(ctx, path, key(1))
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer d.Close()
	if v, _ := d.version(ctx); v != 3 {
		t.Fatalf("version %d want 3", v)
	}
	v, ok := vmByUUID(t, d, "hv1", uuid1, false)
	if !ok || v.DisksUnreadable || v.Name != "vm1" || len(v.Disks) < 10 {
		t.Fatalf("row after upgrade %+v ok=%v", v, ok)
	}
	u := row(uuid1, "vm1")
	u.DisksUnreadable = true
	if _, err := d.ApplyReport(ctx, "hv1", t0.Add(time.Second), "2.6.0", []VMRow{u}); err != nil {
		t.Fatal(err)
	}
	if v, _ := vmByUUID(t, d, "hv1", uuid1, false); !v.DisksUnreadable {
		t.Fatal("flag not stored after upgrade")
	}
}

func TestApplyReportHostsAreIndependent(t *testing.T) {
	ctx := context.Background()
	d, _ := invDB(t)
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	d.ApplyReport(ctx, "hv2", t0, "v", []VMRow{row(uuid2, "vm2")})
	d.ApplyReport(ctx, "hv1", t0.Add(time.Second), "v", nil)
	if _, ok := vmByUUID(t, d, "hv2", uuid2, false); !ok {
		t.Fatal("hv1's report touched hv2")
	}
	inv, _ := d.ListInventory(ctx, "hv2", false)
	if len(inv.Hosts) != 1 || len(inv.VMs) != 1 {
		t.Fatalf("host filter: %+v", inv)
	}
}

func TestApplyReportOrphanCredentialsGetGoneRow(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "vm1", UUID: uuid1, Password: phPassword}))
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "vm2", UUID: uuid2, Password: phPassword}))
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv2", Name: "vm3", UUID: uuid3, Password: phPassword}))
	cnt, err := d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	if err != nil {
		t.Fatal(err)
	}
	if cnt.Listed != 1 || cnt.NewlyGone != 1 {
		t.Fatalf("counts %+v", cnt)
	}
	g, ok := vmByUUID(t, d, "hv1", uuid2, true)
	if !ok || g.Name != "vm2" || g.GoneSince == nil || !g.GoneSince.Equal(c.t) || !g.HasCredentials {
		t.Fatalf("orphan row %+v ok=%v", g, ok)
	}
	if _, ok := vmByUUID(t, d, "hv2", uuid3, true); ok {
		t.Fatal("another host's credential got a row")
	}
	// A second report adds nothing new.
	cnt, _ = d.ApplyReport(ctx, "hv1", t0.Add(time.Second), "v", []VMRow{row(uuid1, "vm1")})
	if cnt.NewlyGone != 0 {
		t.Fatalf("counts %+v", cnt)
	}
}

func TestApplyReportCommentRule(t *testing.T) {
	ctx := context.Background()
	d, _ := invDB(t)
	r := row(uuid1, "vm1")
	r.Comment = str("placeholder comment")
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{r})
	v, _ := vmByUUID(t, d, "hv1", uuid1, false)
	if v.Comment != "placeholder comment" {
		t.Fatalf("set: %q", v.Comment)
	}
	r.Comment = nil // field absent: keep
	d.ApplyReport(ctx, "hv1", t0.Add(time.Second), "v", []VMRow{r})
	v, _ = vmByUUID(t, d, "hv1", uuid1, false)
	if v.Comment != "placeholder comment" {
		t.Fatalf("absent erased it: %q", v.Comment)
	}
	r.Comment = str("") // present and empty: clear
	d.ApplyReport(ctx, "hv1", t0.Add(2*time.Second), "v", []VMRow{r})
	v, _ = vmByUUID(t, d, "hv1", uuid1, false)
	if v.Comment != "" {
		t.Fatalf("clear: %q", v.Comment)
	}
}

func TestApplyReportOrigin(t *testing.T) {
	ctx := context.Background()
	d, _ := invDB(t)
	r := row(uuid1, "vm1")
	r.OriginImage, r.OriginSHA256, r.OriginClone, r.OriginPool = "img1", hash, "thin", "p1"
	r.OriginTemplate = "tmpl1"
	r.Created = "2026-10-31T00:00:00Z"
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{r})
	v, _ := vmByUUID(t, d, "hv1", uuid1, false)
	if v.OriginImage != "img1" || v.OriginSHA256 != hash || v.OriginClone != "thin" ||
		v.OriginPool != "p1" || v.OriginTemplate != "tmpl1" || v.Created != "2026-10-31T00:00:00Z" {
		t.Fatalf("origin %+v", v)
	}
}

func TestEmptyReportMarksGoneDeletesNothing(t *testing.T) {
	ctx := context.Background()
	d, _ := invDB(t)
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "vm1", UUID: uuid1, Password: phPassword}))
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1"), row(uuid2, "vm2")})
	cnt, err := d.ApplyReport(ctx, "hv1", t0.Add(time.Minute), "v", []VMRow{})
	if err != nil {
		t.Fatal(err)
	}
	if cnt.Listed != 0 || cnt.NewlyGone != 2 {
		t.Fatalf("counts %+v", cnt)
	}
	inv, _ := d.ListInventory(ctx, "hv1", true)
	if len(inv.VMs) != 2 {
		t.Fatalf("rows deleted: %+v", inv.VMs)
	}
	for _, v := range inv.VMs {
		if v.GoneSince == nil {
			t.Fatalf("not gone: %+v", v)
		}
	}
	if _, err := d.GetVMCredential(ctx, "hv1", "vm1"); err != nil {
		t.Fatalf("credential: %v", err)
	}
}

func TestDelist(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	c.t = t0.Add(time.Hour)
	got, err := d.Delist(ctx, "hv1", "vm1")
	if err != nil {
		t.Fatal(err)
	}
	if got.UUID != uuid1 || !got.KeptUntil.Equal(c.t.Add(7*24*time.Hour)) {
		t.Fatalf("delist %+v", got)
	}
	g, ok := vmByUUID(t, d, "hv1", uuid1, true)
	if !ok || g.GoneSince == nil || !g.GoneSince.Equal(c.t) {
		t.Fatalf("row %+v", g)
	}
	// No live row now.
	if _, err := d.Delist(ctx, "hv1", "vm1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delist: %v", err)
	}
	if _, err := d.Delist(ctx, "hv1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := d.Delist(ctx, "hv2", "vm1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other host: %v", err)
	}
}

func TestDeleteVMRow(t *testing.T) {
	ctx := context.Background()
	d, _ := invDB(t)
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	if err := d.DeleteVMRow(ctx, "hv1", "vm1", uuid2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong uuid: %v", err)
	}
	if err := d.DeleteVMRow(ctx, "hv1", "vm1", uuid1); err != nil {
		t.Fatal(err)
	}
	if _, ok := vmByUUID(t, d, "hv1", uuid1, true); ok {
		t.Fatal("row remains")
	}
	if err := d.DeleteVMRow(ctx, "hv1", "vm1", uuid1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("again: %v", err)
	}
}

func TestHasCredentialsNeedsSameUUID(t *testing.T) {
	ctx := context.Background()
	d, _ := invDB(t)
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "vm1", UUID: uuid2, Password: phPassword}))
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	v, _ := vmByUUID(t, d, "hv1", uuid1, false)
	if v.HasCredentials {
		t.Fatal("has_credentials with another UUID")
	}
}

func TestSweep(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "vm1", UUID: uuid1, Password: phPassword}))
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	c.t = t0.Add(time.Minute)
	d.ApplyReport(ctx, "hv1", c.t, "v", nil)
	gone := c.t

	n, err := d.Sweep(ctx, gone.Add(6*24*time.Hour+23*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n.Rows != 0 || n.Credentials != 0 {
		t.Fatalf("6d23h swept: %+v", n)
	}
	if _, ok := vmByUUID(t, d, "hv1", uuid1, true); !ok {
		t.Fatal("row removed early")
	}
	if _, err := d.GetVMCredential(ctx, "hv1", "vm1"); err != nil {
		t.Fatal(err)
	}

	n, err = d.Sweep(ctx, gone.Add(7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n.Rows != 1 || n.Credentials != 1 {
		t.Fatalf("7d: %+v", n)
	}
	if _, ok := vmByUUID(t, d, "hv1", uuid1, true); ok {
		t.Fatal("row kept at 7 d")
	}
	if _, err := d.GetVMCredential(ctx, "hv1", "vm1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("credential kept: %v", err)
	}
}

func TestSweepKeepsNewerSameNameCredential(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	c.t = t0.Add(time.Minute)
	d.ApplyReport(ctx, "hv1", c.t, "v", []VMRow{row(uuid2, "vm1")})
	// vm1 was recreated: the credential is the new VM's.
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "vm1", UUID: uuid2, Password: phPassword}))
	n, err := d.Sweep(ctx, c.t.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n.Rows != 1 || n.Credentials != 0 {
		t.Fatalf("sweep %+v", n)
	}
	if _, err := d.GetVMCredential(ctx, "hv1", "vm1"); err != nil {
		t.Fatalf("new VM's credential removed: %v", err)
	}
	if _, ok := vmByUUID(t, d, "hv1", uuid2, false); !ok {
		t.Fatal("live row removed")
	}
}

func TestSweepNeverReadsLastSeenOrLastReport(t *testing.T) {
	ctx := context.Background()
	d, c := invDB(t)
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "vm1", UUID: uuid1, Password: phPassword}))
	d.ApplyReport(ctx, "hv1", t0, "v", []VMRow{row(uuid1, "vm1")})
	c.t = t0.Add(366 * 24 * time.Hour) // the host last reported a year ago
	n, err := d.Sweep(ctx, c.t)
	if err != nil {
		t.Fatal(err)
	}
	if n.Rows != 0 || n.Credentials != 0 {
		t.Fatalf("sweep %+v", n)
	}
	if _, ok := vmByUUID(t, d, "hv1", uuid1, false); !ok {
		t.Fatal("live row removed")
	}
	if _, err := d.GetVMCredential(ctx, "hv1", "vm1"); err != nil {
		t.Fatalf("credential removed: %v", err)
	}
}
