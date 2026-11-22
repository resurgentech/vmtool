package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/store/db"
)

const (
	phS3Key    = "PLACEHOLDER-KEY-not-a-key"
	phPassword = "PLACEHOLDER-not-a-secret-2"
)

var objRE = regexp.MustCompile(`^backups/store-\d{8}T\d{6}Z\.db$`)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type timerReq struct {
	d  time.Duration
	ch chan time.Time
}

// fakeTimers hands each After call to the test, which fires it by hand.
type fakeTimers struct{ reqs chan timerReq }

func newFakeTimers() *fakeTimers { return &fakeTimers{reqs: make(chan timerReq, 16)} }

func (f *fakeTimers) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.reqs <- timerReq{d, ch}
	return ch
}

func (f *fakeTimers) next(t *testing.T) timerReq {
	t.Helper()
	select {
	case r := <-f.reqs:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("scheduler did not ask for a timer")
		return timerReq{}
	}
}

func (f *fakeTimers) none(t *testing.T) {
	t.Helper()
	select {
	case r := <-f.reqs:
		t.Fatalf("unexpected timer %v", r.d)
	case <-time.After(50 * time.Millisecond):
	}
}

// failBlob wraps a BlobStore and fails or blocks chosen calls.
type failBlob struct {
	blob.BlobStore
	putErr, listErr, delErr error
	gate                    chan struct{} // Put waits on it when non-nil
	entered                 chan struct{}
}

func (f *failBlob) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	if f.putErr != nil {
		return f.putErr
	}
	return f.BlobStore.Put(ctx, key, r, size)
}

func (f *failBlob) List(ctx context.Context, prefix string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.BlobStore.List(ctx, prefix)
}

func (f *failBlob) Delete(ctx context.Context, key string) error {
	if f.delErr != nil {
		return f.delErr
	}
	return f.BlobStore.Delete(ctx, key)
}

type bkEnv struct {
	db    *db.DB
	dir   string
	mem   *blob.Memory
	log   *syncBuf
	clock *fakeClock
	deps  BackupDeps
}

func newBkEnv(t *testing.T) *bkEnv {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	d, err := db.Open(context.Background(), filepath.Join(dir, "store.db"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	e := &bkEnv{db: d, dir: dir, mem: blob.NewMemory(), log: &syncBuf{},
		clock: &fakeClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}}
	e.deps = BackupDeps{DB: d, Blob: e.mem, DataDir: dir, Now: e.clock.Now,
		Log: slog.New(slog.NewTextHandler(e.log, nil))}
	if err := d.PutVMCredential(context.Background(), db.VMCredential{
		Host: "hv1", Name: "vm1", UUID: "00000000-0000-4000-8000-000000000001",
		Username: "root", Password: phPassword, PrivateKey: "pk", PublicKey: "pub"}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *bkEnv) names(t *testing.T) []string {
	t.Helper()
	n, err := e.mem.List(context.Background(), "backups/")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *bkEnv) noTemp(t *testing.T) {
	t.Helper()
	ents, err := os.ReadDir(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ents {
		if strings.HasPrefix(x.Name(), ".backup-") {
			t.Errorf("temp file left: %s", x.Name())
		}
	}
}

func (e *bkEnv) seed(t *testing.T, key string) {
	t.Helper()
	if err := e.mem.Put(context.Background(), key, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
}

func TestBackupUploadsAnOpenableCopyWithCiphertextOnly(t *testing.T) {
	e := newBkEnv(t)
	name, err := RunBackup(context.Background(), e.deps)
	if err != nil {
		t.Fatal(err)
	}
	if name != "backups/store-20260301T120000Z.db" {
		t.Fatalf("name = %q", name)
	}
	rc, _, err := e.mem.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	for _, bad := range [][]byte{[]byte(phPassword), bytes.Repeat([]byte{1}, 32)} {
		if bytes.Contains(raw, bad) {
			t.Fatalf("backup holds plaintext secret or key")
		}
	}
	restore := filepath.Join(t.TempDir(), "r")
	if err := os.Mkdir(restore, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(restore, "store.db")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := db.Open(context.Background(), p, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer r.Close()
	c, err := r.GetVMCredential(context.Background(), "hv1", "vm1")
	if err != nil || c.Password != phPassword {
		t.Fatalf("credential read: %v", err)
	}
	e.noTemp(t)
	if !strings.Contains(e.log.String(), name) {
		t.Fatalf("no success line with the object name: %s", e.log.String())
	}
}

func TestBackupRetentionKeepsFourteenOfTwenty(t *testing.T) {
	e := newBkEnv(t)
	for i := 1; i <= 19; i++ {
		e.seed(t, fmt.Sprintf("backups/store-202602%02dT000000Z.db", i))
	}
	foreign := []string{"backups/other.db", "backups/store-x.db", "backups/store-20260201T000000Z.db.bak", "images/a"}
	for _, k := range foreign {
		e.seed(t, k)
	}
	if _, err := RunBackup(context.Background(), e.deps); err != nil {
		t.Fatal(err)
	}
	var valid, foundForeign int
	all, _ := e.mem.List(context.Background(), "")
	for _, k := range all {
		if objRE.MatchString(k) {
			valid++
		}
		for _, f := range foreign {
			if k == f {
				foundForeign++
			}
		}
	}
	if valid != 14 {
		t.Fatalf("kept %d backups, want 14: %v", valid, all)
	}
	if foundForeign != len(foreign) {
		t.Fatalf("a foreign key was deleted: %v", all)
	}
	if _, err := e.mem.Stat(context.Background(), "backups/store-20260301T120000Z.db"); err != nil {
		t.Fatalf("newest backup pruned: %v", err)
	}
	if _, err := e.mem.Stat(context.Background(), "backups/store-20260206T000000Z.db"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a 7th-oldest should be gone: %v", err)
	}
}

func TestBackupFailuresLeaveNoTempFileAndLogOneLine(t *testing.T) {
	secretErr := errors.New("s3 said " + phS3Key + " " + phPassword + " denied")
	cases := []struct {
		name  string
		class string
		setup func(t *testing.T, e *bkEnv)
	}{
		{"put", "put", func(_ *testing.T, e *bkEnv) { e.deps.Blob = &failBlob{BlobStore: e.mem, putErr: secretErr} }},
		{"snapshot", "snapshot", func(_ *testing.T, e *bkEnv) { e.db.Close() }},
		{"list", "list", func(_ *testing.T, e *bkEnv) { e.deps.Blob = &failBlob{BlobStore: e.mem, listErr: secretErr} }},
		{"delete", "delete", func(t *testing.T, e *bkEnv) {
			for i := 1; i <= 15; i++ {
				e.seed(t, fmt.Sprintf("backups/store-202602%02dT000000Z.db", i))
			}
			e.deps.Blob = &failBlob{BlobStore: e.mem, delErr: secretErr}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newBkEnv(t)
			c.setup(t, e)
			_, err := RunBackup(context.Background(), e.deps)
			if err == nil {
				t.Fatal("want an error")
			}
			e.noTemp(t)
			logs := e.log.String()
			if n := strings.Count(strings.TrimSpace(logs), "\n") + 1; n != 1 {
				t.Fatalf("want one log line, got %d:\n%s", n, logs)
			}
			for _, want := range []string{"level=ERROR", "class=" + c.class, "duration="} {
				if !strings.Contains(logs, want) {
					t.Errorf("log lacks %q: %s", want, logs)
				}
			}
			for _, bad := range []string{phS3Key, phPassword, "PLACEHOLDER-not-a-secret-1", strings.Repeat("\x01", 32), "01010101"} {
				if strings.Contains(logs, bad) || strings.Contains(err.Error(), bad) {
					t.Errorf("secret %q leaked: %s / %v", bad, logs, err)
				}
			}
		})
	}
}

func TestBackupSuccessLeavesNoTempFile(t *testing.T) {
	e := newBkEnv(t)
	if _, err := RunBackup(context.Background(), e.deps); err != nil {
		t.Fatal(err)
	}
	e.noTemp(t)
}

func TestBackupRunWhileOneIsInProgressIsSkipped(t *testing.T) {
	e := newBkEnv(t)
	fb := &failBlob{BlobStore: e.mem, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	e.deps.Blob = fb
	b := NewBackups(e.deps)
	done := make(chan error, 1)
	go func() { _, err := b.Run(context.Background()); done <- err }()
	<-fb.entered
	_, err := b.Run(context.Background())
	if !errors.Is(err, ErrBackupBusy) {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(e.log.String(), "skipped") {
		t.Fatalf("no skip line: %s", e.log.String())
	}
	close(fb.gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(e.names(t)); n != 1 {
		t.Fatalf("%d backups, want 1", n)
	}
}

func startSched(t *testing.T, e *bkEnv) (*fakeTimers, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ft := newFakeTimers()
	e.deps.After = ft.After
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ft, cancel, StartBackups(ctx, e.deps)
}

func TestSchedulerRunsAtStartOnlyWhenStaleThenDaily(t *testing.T) {
	e := newBkEnv(t)
	ft, cancel, done := startSched(t, e) // no backup: stale
	r := ft.next(t)
	if r.d != 5*time.Minute {
		t.Fatalf("first wait = %v, want 5m", r.d)
	}
	if len(e.names(t)) != 0 {
		t.Fatal("backed up before the 5 minutes passed")
	}
	e.clock.Add(5 * time.Minute)
	r.ch <- e.clock.Now()
	r = ft.next(t) // run done
	if r.d != 24*time.Hour {
		t.Fatalf("next wait = %v, want 24h", r.d)
	}
	if len(e.names(t)) != 1 {
		t.Fatalf("after the start run: %v", e.names(t))
	}
	e.clock.Add(24 * time.Hour)
	r.ch <- e.clock.Now()
	r = ft.next(t)
	if r.d != 24*time.Hour || len(e.names(t)) != 2 {
		t.Fatalf("after 24h: wait %v names %v", r.d, e.names(t))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop with its context")
	}
	e.noTemp(t)
}

func TestSchedulerSkipsStartRunWhenNewestIsFresh(t *testing.T) {
	e := newBkEnv(t)
	e.seed(t, "backups/store-20260301T110000Z.db") // 1 h old
	ft, _, _ := startSched(t, e)
	r := ft.next(t)
	if r.d != 24*time.Hour {
		t.Fatalf("first wait = %v, want 24h (no start run)", r.d)
	}
	ft.none(t)
	if n := len(e.names(t)); n != 1 {
		t.Fatalf("%d backups, want only the seeded one", n)
	}
}

func TestSchedulerRunsAtStartWhenNewestIsOlderThan24h(t *testing.T) {
	e := newBkEnv(t)
	e.seed(t, "backups/store-20260227T110000Z.db")
	ft, _, _ := startSched(t, e)
	if r := ft.next(t); r.d != 5*time.Minute {
		t.Fatalf("first wait = %v, want 5m", r.d)
	}
}
