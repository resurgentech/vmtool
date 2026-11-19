package db

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	phPassword = "PLACEHOLDER-not-a-secret-2"
	phKeyText  = "PLACEHOLDER-KEY-not-a-key"
	phUUID     = "00000000-0000-4000-8000-000000000001"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "store.db")
	d, err := Open(context.Background(), path, key(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

func TestOpenCreatesPrivateFileAndDir(t *testing.T) {
	_, path := openTemp(t)
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Fatalf("dir: %v %v", di, err)
	}
}

func TestOpenRefusesLooseModes(t *testing.T) {
	ctx := context.Background()
	d, path := openTemp(t)
	d.Close()

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, key(1)); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("loose file: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, sib := range []string{"-wal", "-shm", "-journal"} {
		p := path + sib
		if err := os.WriteFile(p, nil, 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(ctx, path, key(1)); err == nil || !strings.Contains(err.Error(), "permission") {
			t.Fatalf("loose %s: %v", sib, err)
		}
		os.Remove(p)
	}

	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, key(1)); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("loose dir: %v", err)
	}
	os.Chmod(filepath.Dir(path), 0o700)
}

func TestWrongKeyIsClearError(t *testing.T) {
	ctx := context.Background()
	d, path := openTemp(t)
	if err := d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "vm1", UUID: phUUID, Password: phPassword}); err != nil {
		t.Fatal(err)
	}
	d.Close()
	_, err := Open(ctx, path, key(2))
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("want ErrWrongKey, got %v", err)
	}
	if !strings.Contains(err.Error(), "encryption key does not match this database") {
		t.Fatalf("message: %v", err)
	}
	d2, err := Open(ctx, path, key(1))
	if err != nil {
		t.Fatalf("right key: %v", err)
	}
	defer d2.Close()
	got, err := d2.GetVMCredential(ctx, "hv1", "vm1")
	if err != nil || got.Password != phPassword {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestOpenRejectsBadKeyLength(t *testing.T) {
	_, err := Open(context.Background(), filepath.Join(t.TempDir(), "s.db"), []byte("short"))
	if err == nil {
		t.Fatal("want error")
	}
}

func TestMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d", "store.db")
	base := migrations

	d, err := openMigrations(ctx, path, key(1), base)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := d.version(ctx); v != len(base) {
		t.Fatalf("fresh version %d want %d", v, len(base))
	}
	d.Close()

	// Reopen is a no-op.
	d, err = openMigrations(ctx, path, key(1), base)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := d.version(ctx); v != len(base) {
		t.Fatalf("reopen version %d", v)
	}
	d.Close()

	// An older database is migrated forward.
	extra := append(append([]string{}, base...), `CREATE TABLE extra_t (x TEXT)`)
	d, err = openMigrations(ctx, path, key(1), extra)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := d.version(ctx); v != len(extra) {
		t.Fatalf("migrated version %d", v)
	}
	if _, err := d.sql.ExecContext(ctx, `INSERT INTO extra_t(x) VALUES ('a')`); err != nil {
		t.Fatal(err)
	}
	d.Close()

	// A newer database than the code knows is refused.
	_, err = openMigrations(ctx, path, key(1), base)
	if err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("want newer-version refusal, got %v", err)
	}
}

func TestImages(t *testing.T) {
	ctx := context.Background()
	d, _ := openTemp(t)
	if _, err := d.GetImage(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	must(t, d.PutImage(ctx, Image{Name: "a", SHA256: "aa", Size: 10}))
	must(t, d.PutImage(ctx, Image{Name: "b", SHA256: "bb", Size: 20}))
	must(t, d.PutImage(ctx, Image{Name: "a", SHA256: "a2", Size: 11}))
	got, err := d.GetImage(ctx, "a")
	if err != nil || got.SHA256 != "a2" || got.Size != 11 || got.Updated.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	list, err := d.ListImages(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "a" || list[1].Name != "b" {
		t.Fatalf("%+v %v", list, err)
	}
	must(t, d.DeleteImage(ctx, "a"))
	if err := d.DeleteImage(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestVMCredentials(t *testing.T) {
	ctx := context.Background()
	d, _ := openTemp(t)
	if _, err := d.GetVMCredential(ctx, "hv1", "vm1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	c := VMCredential{Host: "hv1", Name: "vm1", UUID: phUUID, Username: "root", Password: phPassword, PrivateKey: phKeyText, PublicKey: "pub"}
	must(t, d.PutVMCredential(ctx, c))
	got, err := d.GetVMCredential(ctx, "hv1", "vm1")
	if err != nil {
		t.Fatal(err)
	}
	c.Updated = got.Updated
	if got != c || got.Updated.IsZero() {
		t.Fatalf("%+v", got)
	}
	// PUT replaces.
	c2 := VMCredential{Host: "hv1", Name: "vm1", UUID: "00000000-0000-4000-8000-000000000002", Username: "u2"}
	must(t, d.PutVMCredential(ctx, c2))
	got, _ = d.GetVMCredential(ctx, "hv1", "vm1")
	if got.UUID != c2.UUID || got.Password != "" || got.Username != "u2" {
		t.Fatalf("%+v", got)
	}
	// Same name on another host is a different row.
	if _, err := d.GetVMCredential(ctx, "hv2", "vm1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	must(t, d.DeleteVMCredential(ctx, "hv1", "vm1"))
	if err := d.DeleteVMCredential(ctx, "hv1", "vm1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestMachines(t *testing.T) {
	ctx := context.Background()
	d, _ := openTemp(t)
	if _, err := d.GetMachine(ctx, "m1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	m := Machine{Name: "m1", Kind: "metal", Address: "PLACEHOLDER-HOST",
		Credentials: Credentials{Username: "root", Password: phPassword, PrivateKey: phKeyText, PublicKey: "pub"}}
	must(t, d.PutMachine(ctx, m))
	must(t, d.PutMachine(ctx, Machine{Name: "m0", Kind: "vm", Address: "hv1"}))
	got, err := d.GetMachine(ctx, "m1")
	if err != nil || got.Credentials != m.Credentials || got.Kind != "metal" || got.Address != m.Address {
		t.Fatalf("%+v %v", got, err)
	}
	list, err := d.ListMachines(ctx)
	if err != nil || len(list) != 2 || list[0] != (MachineSummary{Name: "m0", Kind: "vm"}) || list[1].Name != "m1" {
		t.Fatalf("%+v %v", list, err)
	}
	must(t, d.DeleteMachine(ctx, "m1"))
	if err := d.DeleteMachine(ctx, "m1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestFreshNonceEachWrite(t *testing.T) {
	ctx := context.Background()
	d, _ := openTemp(t)
	c := VMCredential{Host: "hv1", Name: "vm1", Password: phPassword}
	read := func() []byte {
		var b []byte
		if err := d.sql.QueryRowContext(ctx, `SELECT password FROM vm_credentials WHERE host='hv1' AND name='vm1'`).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	must(t, d.PutVMCredential(ctx, c))
	a := read()
	must(t, d.PutVMCredential(ctx, c))
	b := read()
	if bytes.Equal(a, b) || bytes.Equal(a[:12], b[:12]) {
		t.Fatal("ciphertext or nonce repeated")
	}
}

func TestCiphertextBoundToRowAndColumn(t *testing.T) {
	ctx := context.Background()
	d, _ := openTemp(t)
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "a", Password: phPassword}))
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "b", Password: "x"}))
	// Copy a's password ciphertext into b's password.
	if _, err := d.sql.ExecContext(ctx, `UPDATE vm_credentials SET password=(SELECT password FROM vm_credentials WHERE name='a') WHERE name='b'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetVMCredential(ctx, "hv1", "b"); err == nil {
		t.Fatal("swapped ciphertext was accepted")
	}
	// Copy a's password ciphertext into a's username column.
	if _, err := d.sql.ExecContext(ctx, `UPDATE vm_credentials SET username=password WHERE name='a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetVMCredential(ctx, "hv1", "a"); err == nil {
		t.Fatal("column-swapped ciphertext was accepted")
	}
}

func TestRawFilesAndSnapshotHoldNoPlaintextSecret(t *testing.T) {
	ctx := context.Background()
	d, path := openTemp(t)
	must(t, d.PutVMCredential(ctx, VMCredential{Host: "hv1", Name: "vm1", UUID: phUUID, Username: "root", Password: phPassword, PrivateKey: phKeyText, PublicKey: phKeyText}))
	must(t, d.PutMachine(ctx, Machine{Name: "m1", Kind: "metal", Address: "PLACEHOLDER-HOST",
		Credentials: Credentials{Password: phPassword, PrivateKey: phKeyText}}))
	snapDir := filepath.Join(t.TempDir(), "snaps")
	must(t, os.Mkdir(snapDir, 0o700))
	snap := filepath.Join(snapDir, "snap.db")
	must(t, d.Snapshot(ctx, snap))
	if fi, err := os.Stat(snap); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode: %v %v", fi, err)
	}
	if err := d.Snapshot(ctx, snap); err == nil {
		t.Fatal("snapshot over an existing file must fail")
	}
	d.Close()

	files, _ := filepath.Glob(path + "*")
	files = append(files, snap)
	if len(files) < 2 {
		t.Fatalf("files: %v", files)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{phPassword, phKeyText} {
			if bytes.Contains(b, []byte(p)) {
				t.Fatalf("%s holds %q in plain text", f, p)
			}
		}
	}
	// The snapshot opens and reads back under the key.
	s, err := Open(ctx, snap, key(1))
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer s.Close()
	got, err := s.GetVMCredential(ctx, "hv1", "vm1")
	if err != nil || got.Password != phPassword {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	raw := key(1)
	hexs := []byte(strings.Repeat("01", 32))

	for name, data := range map[string][]byte{"raw": raw, "hex": hexs, "hexnl": append(append([]byte{}, hexs...), '\n')} {
		k, err := LoadKey(write(name, data, 0o600))
		if err != nil || !bytes.Equal(k, raw) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := LoadKey(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing")
	}
	if _, err := LoadKey(write("loose", raw, 0o640)); err == nil {
		t.Fatal("group-readable key accepted")
	}
	if _, err := LoadKey(write("loose2", raw, 0o604)); err == nil {
		t.Fatal("other-readable key accepted")
	}
	for name, data := range map[string][]byte{
		"short":  []byte("PLACEHOLDER-KEY-not-a-key"),
		"nothex": []byte(strings.Repeat("zz", 32)),
		"empty":  nil,
	} {
		_, err := LoadKey(write(name, data, 0o600))
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if strings.Contains(err.Error(), "PLACEHOLDER-KEY") || strings.Contains(err.Error(), "zzzz") {
			t.Fatalf("%s: error leaks key bytes: %v", name, err)
		}
	}
}

func TestUpdatedIsRecent(t *testing.T) {
	ctx := context.Background()
	d, _ := openTemp(t)
	must(t, d.PutImage(ctx, Image{Name: "a", SHA256: "aa", Size: 1}))
	got, _ := d.GetImage(ctx, "a")
	if time.Since(got.Updated) > time.Minute {
		t.Fatalf("updated %v", got.Updated)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
