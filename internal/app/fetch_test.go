package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	// blob, if set, is what GET .../blob serves, with X-Image-SHA256 blobSHA.
	blob    []byte
	blobSHA string
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
			if f.blob != nil {
				w.Header().Set("X-Image-SHA256", f.blobSHA)
				w.Header().Set("Content-Length", fmt.Sprint(len(f.blob)))
				_, _ = w.Write(f.blob)
				return
			}
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
	// uploadCalls records every UploadImage; uploadErrs[i] is returned by the
	// i-th call (after the body was read) when set.
	uploadCalls []uploadCall
	uploadErrs  []error
	// noLocal: this machine has no copy of the image.
	noLocal bool
}

type uploadCall struct {
	name, sha string
	size      int64
	body      string
}

func (f *fetchVMs) UploadImage(ctx context.Context, name string, r io.Reader, size int64, sha string, progress func(int64)) error {
	i := len(f.uploadCalls)
	b, _ := io.ReadAll(r)
	f.uploadCalls = append(f.uploadCalls, uploadCall{name, sha, size, string(b)})
	if progress != nil {
		progress(int64(len(b)))
	}
	f.uploads++
	if i < len(f.uploadErrs) && f.uploadErrs[i] != nil {
		return f.uploadErrs[i]
	}
	return nil
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
	if !vms.noLocal {
		r.src.path = local
	}
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
	if r.statuses() != "fetch:done" {
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
	if r.statuses() != "fetch:start,fetch:progress,fetch:progress,fetch:done" {
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
// one *Error.
func TestSyncStoreFetchErrorNeverHasTicket(t *testing.T) {
	for name, ferr := range map[string]error{
		"plain":     errors.New("hv1: disk full, ticket " + imgTicket),
		"relayable": &relayErr{"hv1: store unreachable from hv1: Authorization: Ticket " + imgTicket},
		"app error": &Error{Kind: KindConflict, Op: "fetchImage", Err: &relayErr{"no store configured on hv1 " + imgTicket}},
	} {
		t.Run(name, func(t *testing.T) {
			r := syncStore(t, func(st *imgStore, v *fetchVMs) {
				st.put("u.qcow2", imgSHA, 10)
				// A relayable failure is relayed (S11); with no blob in the
				// store and no local copy the relay fails and so does create.
				v.noLocal = name != "plain"
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
			// A plain failure ends the fetch stage; a relayable one is
			// relayed and the relay is what failed.
			wantStage := "relay"
			if name == "plain" {
				wantStage = "fetch"
			}
			if last.Stage != wantStage || last.Status != "error" {
				t.Errorf("last event %+v", last)
			}
			if r.vms.uploads != 0 {
				t.Error("failed fetch fell through to an upload")
			}
			if name == "plain" && IsRelayableFetch(r.err) {
				t.Errorf("a plain failure became relayable: %v", r.err)
			}
		})
	}
}

// relayKindErr is a relayable fetch failure with the reason the line gives.
type relayKindErr struct{ msg, reason string }

func (e *relayKindErr) Error() string       { return e.msg }
func (e *relayKindErr) Relayable() bool     { return true }
func (e *relayKindErr) RelayReason() string { return e.reason }

type plainFetchErr struct{ msg string }

func (e *plainFetchErr) Error() string { return e.msg }

const relayBytes = "PLACEHOLDER-image-bytes"

// relayStore puts the image in the store with bytes the relay can stream.
func relayStore(st *imgStore) {
	st.put("u.qcow2", imgSHA, int64(len(relayBytes)))
	st.blob = []byte(relayBytes)
	st.blobSHA = imgSHA
}

func relayLines(r *storeRun) []string {
	var out []string
	for _, n := range r.notes {
		if strings.HasPrefix(n, "relaying ") {
			out = append(out, n)
		}
	}
	return out
}

func assertRelayed(t *testing.T, r *storeRun, reason string) {
	t.Helper()
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.vms.uploadCalls) != 1 || r.vms.uploadCalls[0] != (uploadCall{"u.qcow2", imgSHA, int64(len(relayBytes)), relayBytes}) {
		t.Fatalf("uploads %+v", r.vms.uploadCalls)
	}
	want := "relaying u.qcow2 to hv1 through this machine: " + reason
	if got := relayLines(r); len(got) != 1 || got[0] != want {
		t.Fatalf("relay lines %q, want %q", got, want)
	}
	if r.src.n.Load() != 0 {
		t.Errorf("this machine's copy was looked for: %d", r.src.n.Load())
	}
	if ents, _ := os.ReadDir("."); len(ents) != 0 {
		t.Errorf("relay left files in the working directory: %v", ents)
	}
	if !strings.Contains(r.statuses(), "relay:notice") || !strings.HasSuffix(r.statuses(), "relay:progress,relay:done") {
		t.Errorf("events %s", r.statuses())
	}
	for _, e := range r.events {
		if s := fmt.Sprintf("%+v", e); strings.Contains(s, imgTicket) || strings.Contains(s, stTok) {
			t.Errorf("event has a credential: %s", s)
		}
	}
}

// One test per relayable kind: the line names the reason, the store's blob
// is streamed into UploadImage with the store's digest and size, and nothing
// is read from or written to this machine.
func TestSyncStoreRelaysWhenHostCannotFetch(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		reason string
	}{
		"409 no store_url":      {&relayKindErr{"hv1: no store configured on hv1", "no store configured on the host"}, "no store configured on the host"},
		"404 pre-2.3.0 daemon":  {&relayKindErr{"hv1: fetchImage not found", "the host's daemon predates fetchImage"}, "the host's daemon predates fetchImage"},
		"store_unreachable":     {&relayKindErr{"hv1: store unreachable from hv1: dial tcp " + imgTicket, "the store is unreachable from the host"}, "the store is unreachable from the host"},
		"relayable without why": {&relayErr{"hv1: x"}, "the host cannot fetch it from the store"},
	} {
		t.Run(name, func(t *testing.T) {
			r := syncStore(t, func(st *imgStore, v *fetchVMs) {
				relayStore(st)
				v.fetchErr = c.err
			})
			assertRelayed(t, r, c.reason)
			if r.store.blobs.Load() != 1 {
				t.Errorf("blob GETs %d", r.store.blobs.Load())
			}
		})
	}
}

// A mismatch, a refusal and any other failure are not relayed: create fails
// and no byte is streamed.
func TestSyncStoreNoRelayWhenNotRelayable(t *testing.T) {
	for name, ferr := range map[string]error{
		"digest or size mismatch": &plainFetchErr{"hv1: digest mismatch"},
		"store_refused":           &plainFetchErr{"hv1: store refused the ticket"},
	} {
		t.Run(name, func(t *testing.T) {
			r := syncStore(t, func(st *imgStore, v *fetchVMs) { relayStore(st); v.fetchErr = ferr })
			if r.err == nil || !strings.Contains(r.err.Error(), ferr.Error()) {
				t.Fatalf("err %v", r.err)
			}
			if r.store.blobs.Load() != 0 || r.vms.uploads != 0 || r.src.n.Load() != 0 || len(relayLines(r)) != 0 {
				t.Fatalf("blobs %d uploads %d lookups %d lines %q", r.store.blobs.Load(), r.vms.uploads, r.src.n.Load(), r.notes)
			}
		})
	}
}

// The relay fails (the daemon refuses the upload) and a local copy exists:
// local-mode upload with its own line.
func TestSyncStoreRelayFailsFallsBackToLocal(t *testing.T) {
	r := syncStore(t, func(st *imgStore, v *fetchVMs) {
		relayStore(st)
		v.fetchErr = &relayKindErr{"hv1: no store configured on hv1", "no store configured on the host"}
		v.uploadErrs = []error{errors.New("hv1: upload broke")}
	})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.vms.uploadCalls) != 2 || r.vms.uploadCalls[1].body != "image" || r.vms.uploadCalls[1].sha == imgSHA {
		t.Fatalf("uploads %+v", r.vms.uploadCalls)
	}
	var fb []string
	for _, n := range r.notes {
		if strings.Contains(n, "relay") && strings.Contains(n, "using this machine's copy") {
			fb = append(fb, n)
		}
	}
	if len(fb) != 1 || len(r.notes) != 2 {
		t.Fatalf("notes %q", r.notes)
	}
}

// The relay fails and this machine has no copy: create fails, saying so.
func TestSyncStoreRelayFailsNoLocalCopyFails(t *testing.T) {
	r := syncStore(t, func(st *imgStore, v *fetchVMs) {
		relayStore(st)
		v.noLocal = true
		v.fetchErr = &relayKindErr{"hv1: no store configured on hv1", "no store configured on the host"}
		v.uploadErrs = []error{errors.New("hv1: upload broke")}
	})
	if r.err == nil {
		t.Fatal("create succeeded")
	}
	for _, w := range []string{"u.qcow2", "hv1", "relay", "upload broke", "no copy"} {
		if !strings.Contains(r.err.Error(), w) {
			t.Errorf("error lacks %q: %v", w, r.err)
		}
	}
	if len(r.vms.uploadCalls) != 1 {
		t.Errorf("uploads %+v", r.vms.uploadCalls)
	}
	if strings.Contains(r.err.Error(), imgTicket) || strings.Contains(r.err.Error(), stTok) {
		t.Errorf("credential in error: %v", r.err)
	}
	if last := r.events[len(r.events)-1]; last.Status != "error" {
		t.Errorf("last event %+v", last)
	}
}

// The store's blob disagrees with its own metadata: nothing is uploaded from
// it; with a local copy that is used, with a line.
func TestSyncStoreRelayBlobDisagreesDoesNotUpload(t *testing.T) {
	for name, mut := range map[string]func(*imgStore){
		"digest": func(st *imgStore) { st.blobSHA = imgOther },
		"size":   func(st *imgStore) { st.blob = []byte("short") },
	} {
		t.Run(name, func(t *testing.T) {
			r := syncStore(t, func(st *imgStore, v *fetchVMs) {
				relayStore(st)
				mut(st)
				v.fetchErr = &relayKindErr{"hv1: no store configured on hv1", "no store configured on the host"}
			})
			if r.err != nil {
				t.Fatal(r.err)
			}
			if len(r.vms.uploadCalls) != 1 || r.vms.uploadCalls[0].body != "image" {
				t.Fatalf("the store's blob was uploaded: %+v", r.vms.uploadCalls)
			}
		})
	}
}

// stagesRan is the stages the create timer lists for the run's events, each
// stamped 10 ms after the one before, with the ms of each.
func stagesRan(r *storeRun) (names []string, ms map[string]int64) {
	var tm CreateTimer
	for i, e := range r.events {
		e.ElapsedMS = int64(i) * 10
		tm.Add(e)
	}
	ms = map[string]int64{}
	for _, s := range tm.Timings().Stages {
		names = append(names, s.Stage)
		ms[s.Stage] = s.MS
	}
	return names, ms
}

func TestSyncStoreStageNames(t *testing.T) {
	// The host fetches: stage fetch only.
	r := syncStore(t, func(st *imgStore, v *fetchVMs) {
		st.put("u.qcow2", imgSHA, 100)
		v.progress = [][2]int64{{50, 100}, {100, 100}}
	})
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got, _ := stagesRan(r); !reflect.DeepEqual(got, []string{"fetch"}) {
		t.Errorf("fetch: stages %v (%s)", got, r.statuses())
	}

	// The fetch is not possible, this machine relays: fetch, then relay.
	r = syncStore(t, func(st *imgStore, v *fetchVMs) {
		relayStore(st)
		v.fetchErr = &relayKindErr{"hv1: no store configured on hv1", "no store configured on the host"}
	})
	if got, _ := stagesRan(r); !reflect.DeepEqual(got, []string{"fetch", "relay"}) {
		t.Errorf("relay: stages %v (%s)", got, r.statuses())
	}
	for _, e := range r.events {
		if e.Stage == "upload" {
			t.Errorf("relay run has an upload event: %+v", e)
		}
	}

	// The relay fails and this machine's copy goes up: fetch, relay, upload,
	// each timed under its own name.
	r = syncStore(t, func(st *imgStore, v *fetchVMs) {
		relayStore(st)
		v.fetchErr = &relayKindErr{"hv1: no store configured on hv1", "no store configured on the host"}
		v.uploadErrs = []error{errors.New("hv1: upload broke")}
	})
	if r.err != nil {
		t.Fatal(r.err)
	}
	got, ms := stagesRan(r)
	if !reflect.DeepEqual(got, []string{"fetch", "relay", "upload"}) {
		t.Fatalf("fallback: stages %v (%s)", got, r.statuses())
	}
	if ms["relay"] <= 0 || ms["upload"] <= 0 {
		t.Errorf("fallback: timings %v", ms)
	}
	// The upload stage starts at its own hash event, not at a notice.
	for i, e := range r.events {
		if e.Stage == "upload" {
			if e.Status != "hash" {
				t.Errorf("first upload event %d is %s, want hash", i, e.Status)
			}
			break
		}
	}
}
