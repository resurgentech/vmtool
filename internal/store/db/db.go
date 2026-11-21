// Package db is the vmtool-store SQLite database: the image index, VM
// credential records and machine (metal) records. It is pure Go
// (modernc.org/sqlite), so the store builds with CGO_ENABLED=0, and it
// imports neither pkg/vmtool nor internal/config.
//
// Credential columns (vm_credentials.username/password/private_key/
// public_key and machines.credentials) are sealed with AES-256-GCM under a
// 32-byte key. Each write uses a fresh random 12-byte nonce, stored in front
// of the ciphertext (nonce||ct). The GCM additional data names the table,
// column and row key, so a ciphertext copied into another row or column fails
// to open. Host, name, uuid, kind, address and updated stay plain.
//
// Journal mode is SQLite's default rollback journal (DELETE), not WAL: the
// only side file is a transient "-journal", which SQLite creates with the
// database file's 0600 mode, and no "-wal"/"-shm" files exist to leak or to
// check. Open still refuses any of the three siblings with a loose mode.
//
// Nothing here logs a credential or a key; errors name fields, never values.
package db

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	// ErrNotFound is returned when a row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrWrongKey is returned by Open when the key is not the one the
	// database was created with.
	ErrWrongKey = errors.New("encryption key does not match this database")
)

const (
	keyLen   = 32
	keyCheck = "vmtool-store key check v1"
)

// migrations is the ordered, forward-only schema history. Entry i brings the
// database to version i+1. Never edit an entry that has shipped; append.
var migrations = []string{
	`CREATE TABLE meta (
		k TEXT PRIMARY KEY,
		v BLOB NOT NULL
	);
	CREATE TABLE images (
		name    TEXT PRIMARY KEY,
		sha256  TEXT NOT NULL,
		size    INTEGER NOT NULL,
		updated TEXT NOT NULL
	);
	CREATE TABLE vm_credentials (
		host        TEXT NOT NULL,
		name        TEXT NOT NULL,
		uuid        TEXT NOT NULL,
		username    BLOB NOT NULL,
		password    BLOB NOT NULL,
		private_key BLOB NOT NULL,
		public_key  BLOB NOT NULL,
		updated     TEXT NOT NULL,
		PRIMARY KEY (host, name)
	);
	CREATE TABLE machines (
		name        TEXT PRIMARY KEY,
		kind        TEXT NOT NULL,
		address     TEXT NOT NULL,
		credentials BLOB NOT NULL,
		updated     TEXT NOT NULL
	);`,
	// 2: the VM inventory. No secret column; times are RFC3339Nano UTC text.
	`CREATE TABLE hosts (
		name           TEXT PRIMARY KEY,
		last_report    TEXT NOT NULL,
		generated_at   TEXT NOT NULL,
		daemon_version TEXT NOT NULL
	);
	CREATE TABLE vms (
		host            TEXT NOT NULL,
		uuid            TEXT NOT NULL,
		name            TEXT NOT NULL,
		state           TEXT NOT NULL,
		vcpus           INTEGER NOT NULL,
		memory_gib      INTEGER NOT NULL,
		ip              TEXT NOT NULL,
		autostart       INTEGER NOT NULL,
		firmware        TEXT NOT NULL,
		disks           TEXT NOT NULL,
		origin_image    TEXT NOT NULL,
		origin_sha256   TEXT NOT NULL,
		origin_template TEXT NOT NULL,
		origin_clone    TEXT NOT NULL,
		origin_pool     TEXT NOT NULL,
		created         TEXT NOT NULL,
		comment         TEXT NOT NULL,
		first_seen      TEXT NOT NULL,
		last_seen       TEXT NOT NULL,
		gone_since      TEXT,
		PRIMARY KEY (host, uuid)
	);
	CREATE INDEX vms_host_name ON vms(host, name);`,
	// 3: a VM whose disks the host could not read keeps its last known disks
	// and is marked.
	`ALTER TABLE vms ADD COLUMN disks_unreadable INTEGER NOT NULL DEFAULT 0;`,
	// 4: a VM's notes (origin and comment) move out of the reported row into
	// their own table, keyed (host, uuid), which no report touches. Existing
	// values are carried over; the origin counts as written when the row was
	// first seen, the comment when it was last seen.
	`CREATE TABLE vm_notes (
		host            TEXT NOT NULL,
		uuid            TEXT NOT NULL,
		name            TEXT NOT NULL,
		origin_image    TEXT NOT NULL DEFAULT '',
		origin_sha256   TEXT NOT NULL DEFAULT '',
		origin_template TEXT NOT NULL DEFAULT '',
		origin_clone    TEXT NOT NULL DEFAULT '',
		origin_pool     TEXT NOT NULL DEFAULT '',
		origin_created  TEXT NOT NULL DEFAULT '',
		comment         TEXT NOT NULL DEFAULT '',
		origin_written  TEXT,
		comment_updated TEXT,
		PRIMARY KEY (host, uuid)
	);
	INSERT INTO vm_notes(host, uuid, name, origin_image, origin_sha256, origin_template, origin_clone,
		origin_pool, origin_created, comment, origin_written, comment_updated)
	SELECT host, uuid, name, origin_image, origin_sha256, origin_template, origin_clone, origin_pool, created, comment,
		CASE WHEN origin_image<>'' OR origin_sha256<>'' OR origin_template<>'' OR origin_clone<>''
		       OR origin_pool<>'' OR created<>'' THEN first_seen END,
		CASE WHEN comment<>'' THEN last_seen END
	FROM vms
	WHERE origin_image<>'' OR origin_sha256<>'' OR origin_template<>'' OR origin_clone<>''
	   OR origin_pool<>'' OR created<>'' OR comment<>'';
	ALTER TABLE vms DROP COLUMN origin_image;
	ALTER TABLE vms DROP COLUMN origin_sha256;
	ALTER TABLE vms DROP COLUMN origin_template;
	ALTER TABLE vms DROP COLUMN origin_clone;
	ALTER TABLE vms DROP COLUMN origin_pool;
	ALTER TABLE vms DROP COLUMN created;
	ALTER TABLE vms DROP COLUMN comment;`,
	// 5: a VM's cards on vmtool networks, as its host last reported them: a
	// JSON array of {network, mac, ip}, replaced by every report.
	`ALTER TABLE vms ADD COLUMN networks TEXT NOT NULL DEFAULT '[]';`,
	// 6: the network registry. A network's VNI and subnet are unique; its
	// members are the hosts linked to it, with the LAN address and interface
	// of each. Nothing here is secret, and nothing records whether a host is up.
	`CREATE TABLE networks (
		name    TEXT PRIMARY KEY,
		vni     INTEGER NOT NULL UNIQUE,
		subnet  TEXT NOT NULL UNIQUE,
		domain  TEXT NOT NULL,
		home    TEXT NOT NULL,
		created TEXT NOT NULL
	);
	CREATE TABLE network_members (
		network TEXT NOT NULL,
		host    TEXT NOT NULL,
		lan_ip  TEXT NOT NULL,
		lan_dev TEXT NOT NULL,
		joined  TEXT NOT NULL,
		PRIMARY KEY (network, host)
	);
	CREATE INDEX network_members_host ON network_members(host);`,
	// 7: the reservations of networks that span hosts: the copy the store
	// keeps (network, MAC, address, host, VM) of what the home's daemon made.
	// An address is held by one MAC per network. Nothing here is secret.
	`CREATE TABLE network_reservations (
		network TEXT NOT NULL,
		mac     TEXT NOT NULL,
		ip      TEXT NOT NULL,
		host    TEXT NOT NULL,
		vm      TEXT NOT NULL,
		created TEXT NOT NULL,
		PRIMARY KEY (network, mac),
		UNIQUE (network, ip)
	);`,
}

// DB is an open store database. It is safe for concurrent use.
type DB struct {
	sql  *sql.DB
	aead cipher.AEAD
	now  func() time.Time
}

// Image is one image index row.
type Image struct {
	Name    string
	SHA256  string
	Size    int64
	Updated time.Time
}

// VMCredential is the credential record of one VM, keyed by (Host, Name)
// and carrying the libvirt domain UUID.
type VMCredential struct {
	Host       string
	Name       string
	UUID       string
	Username   string
	Password   string
	PrivateKey string
	PublicKey  string
	Updated    time.Time
}

// Credentials are the four credential files shared by VMs and metal.
type Credentials struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
	PublicKey  string `json:"public_key"`
}

// Machine is a metal machine record keyed by Name.
type Machine struct {
	Name        string
	Kind        string
	Address     string
	Credentials Credentials
	Updated     time.Time
}

// MachineSummary is what listing machines returns: names and kinds only.
type MachineSummary struct {
	Name string
	Kind string
}

// LoadKey reads the encryption key file. The file must be a regular file
// with no group or other permission bit, holding exactly 32 raw bytes or 64
// hex characters with an optional trailing newline. Errors never include key
// bytes.
func LoadKey(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("key file %s: not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("key file %s: permission %04o too open, want 0600", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("key file: %w", err)
	}
	defer zero(b)
	if len(b) == keyLen {
		return append([]byte(nil), b...), nil
	}
	h := strings.TrimSuffix(string(b), "\n")
	if len(h) == 2*keyLen {
		k, err := hex.DecodeString(h)
		if err == nil {
			return k, nil
		}
	}
	return nil, fmt.Errorf("key file %s: want 32 raw bytes or 64 hex characters (got %d bytes)", path, len(b))
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Open opens or creates the database at path. The parent directory is
// created 0700 and the file 0600; an existing directory, file or side file
// with a group/other permission bit is refused. It fails with ErrWrongKey if
// key is not the key the database was created with, and refuses a database
// whose schema is newer than this code.
func Open(ctx context.Context, path string, key []byte) (*DB, error) {
	return openMigrations(ctx, path, key, migrations)
}

func openMigrations(ctx context.Context, path string, key []byte, migs []string) (*DB, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("encryption key must be %d bytes (got %d)", keyLen, len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("encryption key rejected")
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, errors.New("encryption key rejected")
	}
	if err := preparePaths(path); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)"}
	sdb, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	sdb.SetMaxOpenConns(1)
	d := &DB{sql: sdb, aead: aead, now: time.Now}
	if err := d.init(ctx, migs); err != nil {
		sdb.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) init(ctx context.Context, migs []string) error {
	v, err := d.version(ctx)
	if err != nil {
		return err
	}
	if v > len(migs) {
		return fmt.Errorf("database schema version %d is newer than this program knows (%d)", v, len(migs))
	}
	if v > 0 {
		if err := d.checkKey(ctx); err != nil {
			return err
		}
	}
	for i := v; i < len(migs); i++ {
		if err := d.migrate(ctx, i+1, migs[i]); err != nil {
			return err
		}
	}
	if v == 0 {
		// A fresh database records the key check; a later Open verifies it.
		sealed, err := d.seal([]byte(keyCheck), "meta|key_check")
		if err != nil {
			return err
		}
		if _, err := d.sql.ExecContext(ctx, `INSERT INTO meta(k, v) VALUES ('key_check', ?)`, sealed); err != nil {
			return fmt.Errorf("record key check: %w", err)
		}
	}
	return nil
}

func (d *DB) version(ctx context.Context) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("read schema: %w", err)
	}
	if n == 0 {
		return 0, nil
	}
	var v sql.NullInt64
	if err := d.sql.QueryRowContext(ctx, `SELECT max(version) FROM schema_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return int(v.Int64), nil
}

func (d *DB) migrate(ctx context.Context, version int, stmt string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL, applied TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("migration %d: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("migration %d: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version(version, applied) VALUES (?, ?)`, version, d.now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("migration %d: %w", version, err)
	}
	return tx.Commit()
}

func (d *DB) checkKey(ctx context.Context) error {
	var sealed []byte
	err := d.sql.QueryRowContext(ctx, `SELECT v FROM meta WHERE k='key_check'`).Scan(&sealed)
	if err != nil {
		return fmt.Errorf("read key check: %w", err)
	}
	pt, err := d.open(sealed, "meta|key_check")
	if err != nil || string(pt) != keyCheck {
		return ErrWrongKey
	}
	return nil
}

// preparePaths creates the directory 0700 and file 0600 if missing, and
// refuses any existing one with a group/other bit.
func preparePaths(path string) error {
	dir := filepath.Dir(path)
	di, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create data directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod data directory: %w", err)
		}
	case err != nil:
		return fmt.Errorf("data directory: %w", err)
	case !di.IsDir():
		return fmt.Errorf("data directory %s: not a directory", dir)
	case di.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("data directory %s: permission %04o too open, want 0700", dir, di.Mode().Perm())
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("create database file: %w", err)
		}
		f.Close()
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("chmod database file: %w", err)
		}
	case err != nil:
		return fmt.Errorf("database file: %w", err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		fi, err = os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("database file: %w", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("%s: permission %04o too open, want 0600", p, fi.Mode().Perm())
		}
	}
	return nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sql.Close() }

// Snapshot writes a consistent copy of the database (VACUUM INTO) to path,
// which must not exist. The file is created 0600. The copy holds the same
// ciphertext, so it is only readable under the same key.
func (d *DB) Snapshot(ctx context.Context, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("snapshot file: %w", err)
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		os.Remove(path)
		return fmt.Errorf("snapshot file: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		os.Remove(path)
		return fmt.Errorf("snapshot: %w", err)
	}
	return nil
}

// SetClock replaces the clock the database reads for stamps and for the
// inventory rules (the store passes its Deps.Now). Call it before use.
func (d *DB) SetClock(now func() time.Time) { d.now = now }

func (d *DB) stamp() string { return d.now().UTC().Format(time.RFC3339Nano) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// seal returns nonce||ciphertext for plaintext bound to aad.
func (d *DB) seal(plaintext []byte, aad string) ([]byte, error) {
	nonce := make([]byte, d.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return d.aead.Seal(nonce, nonce, plaintext, []byte(aad)), nil
}

func (d *DB) open(sealed []byte, aad string) ([]byte, error) {
	n := d.aead.NonceSize()
	if len(sealed) < n+d.aead.Overhead() {
		return nil, errors.New("ciphertext too short")
	}
	return d.aead.Open(nil, sealed[:n], sealed[n:], []byte(aad))
}

func aadVM(col, host, name string) string {
	return "vm_credentials|" + col + "|" + strconv.Quote(host) + "|" + strconv.Quote(name)
}

func aadMachine(name string) string {
	return "machines|credentials|" + strconv.Quote(name)
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PutImage inserts or replaces the image named img.Name.
func (d *DB) PutImage(ctx context.Context, img Image) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO images(name, sha256, size, updated) VALUES (?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET sha256=excluded.sha256, size=excluded.size, updated=excluded.updated`,
		img.Name, img.SHA256, img.Size, d.stamp())
	if err != nil {
		return fmt.Errorf("put image: %w", err)
	}
	return nil
}

// GetImage returns the image named name, or ErrNotFound.
func (d *DB) GetImage(ctx context.Context, name string) (Image, error) {
	var img Image
	var upd string
	err := d.sql.QueryRowContext(ctx, `SELECT name, sha256, size, updated FROM images WHERE name=?`, name).
		Scan(&img.Name, &img.SHA256, &img.Size, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return Image{}, ErrNotFound
	}
	if err != nil {
		return Image{}, fmt.Errorf("get image: %w", err)
	}
	img.Updated = parseTime(upd)
	return img, nil
}

// ListImages returns every image ordered by name.
func (d *DB) ListImages(ctx context.Context) ([]Image, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT name, sha256, size, updated FROM images ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list images: %w", err)
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		var img Image
		var upd string
		if err := rows.Scan(&img.Name, &img.SHA256, &img.Size, &upd); err != nil {
			return nil, fmt.Errorf("list images: %w", err)
		}
		img.Updated = parseTime(upd)
		out = append(out, img)
	}
	return out, rows.Err()
}

// DeleteImage removes the image named name, or returns ErrNotFound.
func (d *DB) DeleteImage(ctx context.Context, name string) error {
	return affected(d.sql.ExecContext(ctx, `DELETE FROM images WHERE name=?`, name))
}

// PutVMCredential inserts or replaces the record for (c.Host, c.Name).
func (d *DB) PutVMCredential(ctx context.Context, c VMCredential) error {
	cols := []struct{ col, val string }{
		{"username", c.Username}, {"password", c.Password},
		{"private_key", c.PrivateKey}, {"public_key", c.PublicKey},
	}
	sealed := make([][]byte, len(cols))
	for i, x := range cols {
		s, err := d.seal([]byte(x.val), aadVM(x.col, c.Host, c.Name))
		if err != nil {
			return fmt.Errorf("put vm credential: seal %s: %w", x.col, err)
		}
		sealed[i] = s
	}
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO vm_credentials(host, name, uuid, username, password, private_key, public_key, updated)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(host, name) DO UPDATE SET uuid=excluded.uuid, username=excluded.username,
		   password=excluded.password, private_key=excluded.private_key,
		   public_key=excluded.public_key, updated=excluded.updated`,
		c.Host, c.Name, c.UUID, sealed[0], sealed[1], sealed[2], sealed[3], d.stamp())
	if err != nil {
		return fmt.Errorf("put vm credential: %w", err)
	}
	return nil
}

// GetVMCredential returns the record for (host, name), or ErrNotFound.
func (d *DB) GetVMCredential(ctx context.Context, host, name string) (VMCredential, error) {
	c := VMCredential{Host: host, Name: name}
	var user, pass, priv, pub []byte
	var upd string
	err := d.sql.QueryRowContext(ctx,
		`SELECT uuid, username, password, private_key, public_key, updated FROM vm_credentials WHERE host=? AND name=?`,
		host, name).Scan(&c.UUID, &user, &pass, &priv, &pub, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return VMCredential{}, ErrNotFound
	}
	if err != nil {
		return VMCredential{}, fmt.Errorf("get vm credential: %w", err)
	}
	for _, x := range []struct {
		col string
		in  []byte
		out *string
	}{{"username", user, &c.Username}, {"password", pass, &c.Password}, {"private_key", priv, &c.PrivateKey}, {"public_key", pub, &c.PublicKey}} {
		pt, err := d.open(x.in, aadVM(x.col, host, name))
		if err != nil {
			return VMCredential{}, fmt.Errorf("get vm credential: decrypt %s: %w", x.col, errDecrypt)
		}
		*x.out = string(pt)
	}
	c.Updated = parseTime(upd)
	return c, nil
}

var errDecrypt = errors.New("authentication failed")

// DeleteVMCredential removes the record for (host, name), or returns ErrNotFound.
func (d *DB) DeleteVMCredential(ctx context.Context, host, name string) error {
	return affected(d.sql.ExecContext(ctx, `DELETE FROM vm_credentials WHERE host=? AND name=?`, host, name))
}

// PutMachine inserts or replaces the machine named m.Name.
func (d *DB) PutMachine(ctx context.Context, m Machine) error {
	blob, err := json.Marshal(m.Credentials)
	if err != nil {
		return errors.New("put machine: encode credentials")
	}
	sealed, err := d.seal(blob, aadMachine(m.Name))
	zero(blob)
	if err != nil {
		return fmt.Errorf("put machine: %w", err)
	}
	_, err = d.sql.ExecContext(ctx,
		`INSERT INTO machines(name, kind, address, credentials, updated) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET kind=excluded.kind, address=excluded.address,
		   credentials=excluded.credentials, updated=excluded.updated`,
		m.Name, m.Kind, m.Address, sealed, d.stamp())
	if err != nil {
		return fmt.Errorf("put machine: %w", err)
	}
	return nil
}

// GetMachine returns the machine named name, or ErrNotFound.
func (d *DB) GetMachine(ctx context.Context, name string) (Machine, error) {
	m := Machine{Name: name}
	var sealed []byte
	var upd string
	err := d.sql.QueryRowContext(ctx, `SELECT kind, address, credentials, updated FROM machines WHERE name=?`, name).
		Scan(&m.Kind, &m.Address, &sealed, &upd)
	if errors.Is(err, sql.ErrNoRows) {
		return Machine{}, ErrNotFound
	}
	if err != nil {
		return Machine{}, fmt.Errorf("get machine: %w", err)
	}
	pt, err := d.open(sealed, aadMachine(name))
	if err != nil {
		return Machine{}, fmt.Errorf("get machine: decrypt credentials: %w", errDecrypt)
	}
	defer zero(pt)
	if err := json.Unmarshal(pt, &m.Credentials); err != nil {
		return Machine{}, errors.New("get machine: decode credentials")
	}
	m.Updated = parseTime(upd)
	return m, nil
}

// ListMachines returns the name and kind of every machine, ordered by name.
func (d *DB) ListMachines(ctx context.Context) ([]MachineSummary, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT name, kind FROM machines ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	defer rows.Close()
	var out []MachineSummary
	for rows.Next() {
		var s MachineSummary
		if err := rows.Scan(&s.Name, &s.Kind); err != nil {
			return nil, fmt.Errorf("list machines: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteMachine removes the machine named name, or returns ErrNotFound.
func (d *DB) DeleteMachine(ctx context.Context, name string) error {
	return affected(d.sql.ExecContext(ctx, `DELETE FROM machines WHERE name=?`, name))
}
