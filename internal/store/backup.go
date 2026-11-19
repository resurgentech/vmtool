package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/jaredeh/vmtool/internal/store/blob"
	"github.com/jaredeh/vmtool/internal/store/db"
)

const (
	backupPrefix = "backups/store-"
	backupStamp  = "20060102T150405Z"
	// BackupKeep is how many backups retention leaves in the bucket.
	BackupKeep = 14
	// BackupInterval is the period of the scheduled backup.
	BackupInterval = 24 * time.Hour
	// BackupStartDelay is the wait before the start-up backup, taken only
	// when the newest backup is older than BackupInterval or there is none.
	BackupStartDelay = 5 * time.Minute
)

// backupNameRE is the only shape of key retention may delete.
var backupNameRE = regexp.MustCompile(`^backups/store-(\d{8}T\d{6}Z)\.db$`)

// ErrBackupBusy means a backup was already running, so this one was skipped.
var ErrBackupBusy = errors.New("backup already in progress")

// BackupError is a failed backup step. Its text is the fixed step name only:
// the underlying error (reachable with errors.Is/As) can quote an S3 key or a
// path, so it is never printed or logged.
type BackupError struct {
	Class string // snapshot, put, list or delete
	Err   error
}

func (e *BackupError) Error() string { return "backup " + e.Class + " failed" }
func (e *BackupError) Unwrap() error { return e.Err }

// BackupDeps are what a backup needs. Now and After default to the real
// clock; tests inject fakes so no test waits.
type BackupDeps struct {
	DB      *db.DB
	Blob    blob.BlobStore
	DataDir string
	Log     *slog.Logger
	Now     func() time.Time
	After   func(time.Duration) <-chan time.Time
}

// Backups runs backups one at a time.
type Backups struct {
	d  BackupDeps
	mu sync.Mutex
}

// NewBackups fills the defaults in d.
func NewBackups(d BackupDeps) *Backups {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.After == nil {
		d.After = time.After
	}
	return &Backups{d: d}
}

// RunBackup makes one backup with retention; it is what the CLI calls. It
// returns the object name.
func RunBackup(ctx context.Context, d BackupDeps) (string, error) {
	return NewBackups(d).Run(ctx)
}

// Run takes one backup: VACUUM INTO a private temp file in the data
// directory, stream it to backups/store-<UTC stamp>.db, remove the temp file
// in every outcome, then prune to the newest BackupKeep. A run while another
// is in progress returns ErrBackupBusy after one log line.
func (b *Backups) Run(ctx context.Context) (string, error) {
	if !b.mu.TryLock() {
		b.d.Log.Info("backup skipped: one is already in progress")
		return "", ErrBackupBusy
	}
	defer b.mu.Unlock()
	start := time.Now()
	name := backupPrefix + b.d.Now().UTC().Format(backupStamp) + ".db"
	fail := func(err error) (string, error) {
		class := "unknown"
		var be *BackupError
		if errors.As(err, &be) {
			class = be.Class
		}
		b.d.Log.Error("backup failed", "op", "backup", "object", name, "class", class,
			"duration", time.Since(start).Round(time.Millisecond))
		return name, err
	}
	size, err := b.snapshotAndPut(ctx, name)
	if err != nil {
		return fail(err)
	}
	if err := b.prune(ctx); err != nil {
		return fail(err)
	}
	b.d.Log.Info("backup done", "op", "backup", "object", name, "size", size,
		"duration", time.Since(start).Round(time.Millisecond))
	return name, nil
}

// snapshotAndPut is one temp-file round trip. Any error is a *BackupError.
func (b *Backups) snapshotAndPut(ctx context.Context, name string) (size int64, err error) {
	var r [6]byte
	if _, err := rand.Read(r[:]); err != nil {
		return 0, &BackupError{Class: "snapshot", Err: err}
	}
	tmp := filepath.Join(b.d.DataDir, ".backup-"+b.d.Now().UTC().Format(backupStamp)+"-"+hex.EncodeToString(r[:])+".db")
	defer os.Remove(tmp)
	if err := b.d.DB.Snapshot(ctx, tmp); err != nil {
		return 0, &BackupError{Class: "snapshot", Err: err}
	}
	f, err := os.Open(tmp)
	if err != nil {
		return 0, &BackupError{Class: "snapshot", Err: err}
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, &BackupError{Class: "snapshot", Err: err}
	}
	if err := b.d.Blob.Put(ctx, name, f, fi.Size()); err != nil {
		return 0, &BackupError{Class: "put", Err: err}
	}
	return fi.Size(), nil
}

// prune deletes every backup beyond the newest BackupKeep. Only keys of the
// exact shape backups/store-<stamp>.db are ever deleted. It tries every
// delete and reports the first failure.
func (b *Backups) prune(ctx context.Context) error {
	names, err := b.list(ctx)
	if err != nil {
		return &BackupError{Class: "list", Err: err}
	}
	if len(names) <= BackupKeep {
		return nil
	}
	var first error
	for _, k := range names[:len(names)-BackupKeep] {
		if err := b.d.Blob.Delete(ctx, k); err != nil && first == nil {
			first = &BackupError{Class: "delete", Err: err}
		}
	}
	return first
}

// list returns the backup objects, oldest first.
func (b *Backups) list(ctx context.Context) ([]string, error) {
	all, err := b.d.Blob.List(ctx, backupPrefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, k := range all {
		if backupNameRE.MatchString(k) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names, nil
}

// stale reports whether the newest backup is older than BackupInterval, or
// there is none, or the bucket cannot be listed.
func (b *Backups) stale(ctx context.Context) bool {
	names, err := b.list(ctx)
	if err != nil || len(names) == 0 {
		return true
	}
	m := backupNameRE.FindStringSubmatch(names[len(names)-1])
	t, err := time.Parse(backupStamp, m[1])
	if err != nil {
		return true
	}
	return b.d.Now().Sub(t) > BackupInterval
}

// Start runs the schedule until ctx ends: a backup every BackupInterval, and
// one BackupStartDelay after start when the newest backup is stale. The
// returned channel closes when the goroutine has stopped.
func (b *Backups) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait := BackupInterval
		if b.stale(ctx) {
			wait = BackupStartDelay
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-b.d.After(wait):
			}
			_, _ = b.Run(ctx) // failures are logged by Run; the next tick tries again
			wait = BackupInterval
		}
	}()
	return done
}

// StartBackups starts the schedule on a new Backups.
func StartBackups(ctx context.Context, d BackupDeps) <-chan struct{} {
	return NewBackups(d).Start(ctx)
}
