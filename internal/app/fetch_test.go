package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/storeapi"
	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const (
	imgTicket = "PLACEHOLDER-ticket-not-a-secret"
	imgSHA    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imgOther  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// imgStore is an in-process store on loopback TLS with the image routes.
type imgStore struct {
	ts      *httptest.Server
	mu      sync.Mutex
	images  map[string]storeapi.Image
	getCode int // when set, GET /v1/images/{name} answers it
	gets    atomic.Int32
	tickets atomic.Int32
	blobs   atomic.Int32
}

func newImgStore(t *testing.T) *imgStore {
	t.Helper()
	f := &imgStore{images: map[string]storeapi.Image{}}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		if r.Header.Get("Authorization") != "Bearer "+stTok {
			out(401, map[string]string{"error": "unauthorized"})
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/v1/images/")
		switch {
		case r.URL.Path == "/v1/whoami":
			out(200, map[string]string{"name": "jared"})
		case ok && strings.HasSuffix(rest, "/tickets"):
			f.tickets.Add(1)
			f.mu.Lock()
			img, have := f.images[strings.TrimSuffix(rest, "/tickets")]
			f.mu.Unlock()
			if !have {
				out(404, map[string]string{"error": "not found"})
				return
			}
			out(201, storeapi.Ticket{Ticket: imgTicket, ExpiresAt: time.Now().Add(15 * time.Minute), Image: img.Name, Sha256: img.Sha256, Size: img.Size})
		case ok && strings.HasSuffix(rest, "/blob"):
			f.blobs.Add(1)
			out(200, map[string]string{})
		case ok:
			f.gets.Add(1)
			if f.getCode != 0 {
				out(f.getCode, map[string]string{"error": "boom"})
				return
			}
			f.mu.Lock()
			img, have := f.images[rest]
			f.mu.Unlock()
			if !have {
				out(404, map[string]string{"error": "not found"})
				return
			}
			out(200, img)
		default:
			out(404, map[string]string{"error": "not found"})
		}
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *imgStore) put(name, sha string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[name] = storeapi.Image{Name: name, Sha256: sha, Size: size, Updated: time.Now()}
}

func (f *imgStore) session(t *testing.T, notify func(string)) *StoreSession {
	t.Helper()
	pool := f.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return OpenStoreSession(f.ts.URL, func() (string, error) { return stTok, nil },
		[]storeclient.Option{storeclient.WithRootCAs(pool)}, notify)
}

// fetchVMs is a host that caches images and can fetch from the store.
type fetchVMs struct {
	storeVMs
	fetches []fetchCall
	// fetchErr, if set, is returned after the progress calls.
	fetchErr error
	// progress is the (received, total) pairs the fake daemon reports.
	progress [][2]int64
}

type fetchCall struct {
	name, ticket, sha string
	size              int64
}

func (f *fetchVMs) FetchFromStore(_ context.Context, name, ticket, sha string, size int64, progress func(int64, int64)) error {
	f.fetches = append(f.fetches, fetchCall{name, ticket, sha, size})
	for _, p := range f.progress {
		progress(p[0], p[1])
	}
	return f.fetchErr
}

// countingSource is an ImageSource that counts its lookups.
type countingSource struct {
	n    atomic.Int32
	path string
}

func (c *countingSource) FindImage(name string) (string, error) {
	c.n.Add(1)
	if c.path == "" {
		return "", fmt.Errorf("%s: %w", name, vmtool.ErrNotFound)
	}
	return c.path, nil
}

type storeRun struct {
	svc    *Service
	vms    *fetchVMs
	store  *imgStore
	src    *countingSource
	events []ProgressEvent
	notes  []string
	err    error
}

// syncStore runs syncImage in store mode against a store double.
func syncStore(t *testing.T, setup func(*imgStore, *fetchVMs)) *storeRun {
	t.Helper()
	t.Chdir(t.TempDir())
	st := newImgStore(t)
	vms := &fetchVMs{}
	setup(st, vms)
	r := &storeRun{store: st, vms: vms, src: &countingSource{}}
	local := filepath.Join(t.TempDir(), "u.qcow2")
	if err := os.WriteFile(local, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.src.path = local
	r.svc = &Service{
		Images:         r.src,
		ImageCachePath: filepath.Join(t.TempDir(), vmtool.ImageManifestFile),
		Store:          st.session(t, func(l string) { r.notes = append(r.notes, l) }),
	}
	in := CreateVMInput{Host: "hv1", Name: "web", Image: "u.qcow2",
		OnProgress: func(ev ProgressEvent) { r.events = append(r.events, ev) }}
	r.err = r.svc.syncImage(context.Background(), vms, in)
	return r
}

func (r *storeRun) statuses() string {
	var s []string
	for _, e := range r.events {
		s = append(s, e.Stage+":"+e.Status)
	}
	return strings.Join(s, ",")
}

// The daemon's manifest already has the store's digest: nothing happens. No
// ticket is minted, nothing is fetched or uploaded, and this machine's copy
// is not even looked for.
func TestSyncStoreHostCurrentDoesNothing(t *testing.T) {
	r := syncStore(t, func(st *imgStore, v *fetchVMs) {
		st.put("u.qcow2", imgSHA, 100)
		v.remote = RemoteImage{SHA256: imgSHA, Size: 100}
	})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.store.tickets.Load() != 0 || len(r.vms.fetches) != 0 || r.vms.uploads != 0 || r.src.n.Load() != 0 {
		t.Fatalf("tickets %d fetches %d uploads %d local lookups %d", r.store.tickets.Load(), len(r.vms.fetches), r.vms.uploads, r.src.n.Load())
	}
	if r.statuses() != "upload:done" {
		t.Errorf("events %s", r.statuses())
	}
}

// A differing digest: a ticket, then fetchImage with it; the daemon's
// progress becomes upload progress events; the client reads no image bytes
// (no local lookup, no blob GET, no upload).
func TestSyncStoreFetchesWithTicketAndReadsNoBytes(t *testing.T) {
	r := syncStore(t, func(st *imgStore, v *fetchVMs) {
		st.put("u.qcow2", imgSHA, 1000)
		v.remote = RemoteImage{SHA256: imgOther, Size: 1000}
		v.progress = [][2]int64{{400, 1000}, {1000, 1000}}
	})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.vms.fetches) != 1 || r.vms.fetches[0] != (fetchCall{"u.qcow2", imgTicket, imgSHA, 1000}) {
		t.Fatalf("fetches %+v", r.vms.fetches)
	}
	if r.src.n.Load() != 0 || r.store.blobs.Load() != 0 || r.vms.uploads != 0 {
		t.Fatalf("client touched image bytes: lookups %d blob GETs %d uploads %d", r.src.n.Load(), r.store.blobs.Load(), r.vms.uploads)
	}
	if r.statuses() != "upload:start,upload:progress,upload:progress,upload:done" {
		t.Fatalf("events %s", r.statuses())
	}
	p := r.events[1]
	if p.Sent != 400 || p.Total != 1000 {
		t.Errorf("progress event %+v", p)
	}
	// A host that has no copy at all is fetched for too.
	r = syncStore(t, func(st *imgStore, v *fetchVMs) { st.put("u.qcow2", imgSHA, 10) })
	if r.err != nil || len(r.vms.fetches) != 1 || r.vms.uploads != 0 {
		t.Fatalf("empty host: err %v fetches %d uploads %d", r.err, len(r.vms.fetches), r.vms.uploads)
	}
}

// The store lacks the image: this machine's copy is uploaded as in local
// mode, with one line suggesting `images push`.
func TestSyncStoreLacksUploadsWithHint(t *testing.T) {
	r := syncStore(t, func(st *imgStore, v *fetchVMs) {})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.vms.uploads != 1 || len(r.vms.fetches) != 0 || r.store.tickets.Load() != 0 {
		t.Fatalf("uploads %d fetches %d tickets %d", r.vms.uploads, len(r.vms.fetches), r.store.tickets.Load())
	}
	if len(r.notes) != 1 || !strings.Contains(r.notes[0], "vmtool images push") || !strings.Contains(r.notes[0], "u.qcow2") {
		t.Fatalf("notes %q", r.notes)
	}
	if !strings.Contains(r.statuses(), "upload:notice") {
		t.Errorf("no notice event: %s", r.statuses())
	}
}

// The store fails after the mode was decided: no fetch can start, so this
// machine's copy is used, with a line saying so.
func TestSyncStoreFailureFallsBackToLocalCopy(t *testing.T) {
	r := syncStore(t, func(st *imgStore, v *fetchVMs) {
		st.put("u.qcow2", imgSHA, 10)
		st.getCode = 503
	})
	if r.err != nil || r.vms.uploads != 1 || len(r.vms.fetches) != 0 {
		t.Fatalf("err %v uploads %d fetches %d", r.err, r.vms.uploads, len(r.vms.fetches))
	}
	if len(r.notes) != 1 || !strings.Contains(r.notes[0], "using this machine's copy") {
		t.Fatalf("notes %q", r.notes)
	}
}

// A foreign image is refused before the store is asked anything.
func TestSyncStoreForeignRefusedFirst(t *testing.T) {
	r := syncStore(t, func(st *imgStore, v *fetchVMs) {
		st.put("u.qcow2", imgSHA, 10)
		v.remote = RemoteImage{Foreign: true, Path: "/p/u.qcow2"}
	})
	var ae *Error
	if !errors.As(r.err, &ae) || ae.Kind != KindConflict {
		t.Fatalf("err %v", r.err)
	}
	if r.store.gets.Load() != 0 || r.store.tickets.Load() != 0 || len(r.vms.fetches) != 0 {
		t.Fatal("the store was asked")
	}
}

// Local mode (no session) is W4's path: no fetch even though the host can.
func TestSyncLocalModeIgnoresFetcher(t *testing.T) {
	t.Chdir(t.TempDir())
	vms := &fetchVMs{}
	local := filepath.Join(t.TempDir(), "u.qcow2")
	if err := os.WriteFile(local, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Images: ImageSourceFunc(func(string) (string, error) { return local, nil }),
		ImageCachePath: filepath.Join(t.TempDir(), vmtool.ImageManifestFile)}
	var evs []ProgressEvent
	err := svc.syncImage(context.Background(), vms, CreateVMInput{Host: "hv1", Image: "u.qcow2", OnProgress: func(e ProgressEvent) { evs = append(evs, e) }})
	if err != nil || vms.uploads != 1 || len(vms.fetches) != 0 {
		t.Fatalf("err %v uploads %d fetches %d", err, vms.uploads, len(vms.fetches))
	}
	for _, e := range evs {
		if e.Status == "notice" {
			t.Errorf("local mode printed %+v", e)
		}
	}
}

type relayErr struct{ msg string }

func (e *relayErr) Error() string   { return e.msg }
func (e *relayErr) Relayable() bool { return true }

// A failing fetch: the error and every event are free of the ticket, even
// when the host's own message carries it; the failure is one stage error and
// one *Error, and the relayable type survives for S11.
func TestSyncStoreFetchErrorNeverHasTicket(t *testing.T) {
	for name, ferr := range map[string]error{
		"plain":     errors.New("hv1: disk full, ticket " + imgTicket),
		"relayable": &relayErr{"hv1: store unreachable from hv1: Authorization: Ticket " + imgTicket},
		"app error": &Error{Kind: KindConflict, Op: "fetchImage", Err: &relayErr{"no store configured on hv1 " + imgTicket}},
	} {
		t.Run(name, func(t *testing.T) {
			r := syncStore(t, func(st *imgStore, v *fetchVMs) {
				st.put("u.qcow2", imgSHA, 10)
				v.fetchErr = ferr
				v.progress = [][2]int64{{5, 10}}
			})
			if r.err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(r.err.Error(), imgTicket) {
				t.Errorf("error has the ticket: %v", r.err)
			}
			for _, e := range r.events {
				if strings.Contains(fmt.Sprintf("%+v", e), imgTicket) {
					t.Errorf("event has the ticket: %+v", e)
				}
			}
			for _, n := range r.notes {
				if strings.Contains(n, imgTicket) {
					t.Errorf("notice has the ticket: %q", n)
				}
			}
			last := r.events[len(r.events)-1]
			if last.Stage != "upload" || last.Status != "error" {
				t.Errorf("last event %+v", last)
			}
			if r.vms.uploads != 0 {
				t.Error("failed fetch fell through to an upload")
			}
			if name != "plain" && !IsRelayableFetch(r.err) {
				t.Errorf("relayable type lost: %v", r.err)
			}
		})
	}
}
