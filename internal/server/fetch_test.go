package server

// fetchImage tests. The store is an in-process httptest TLS server on
// loopback; no real store, S3 or network is touched. The ticket is a
// placeholder and must never leave the daemon except in the one
// Authorization header to the store.

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/remote"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

const fetchTicket = "PLACEHOLDER-not-a-secret-1"

// testStore is a store stand-in serving GET /v1/images/<name>/blob. It
// records every request it gets.
type testStore struct {
	srv     *httptest.Server
	pool    *x509.CertPool
	handler http.HandlerFunc
	mu      sync.Mutex
	reqs    []*http.Request // headers and URL only
}

func newTestStore(t *testing.T, h http.HandlerFunc) *testStore {
	t.Helper()
	s := &testStore{handler: h}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Clone(context.Background()))
		s.mu.Unlock()
		s.handler(w, r)
	}))
	t.Cleanup(s.srv.Close)
	s.pool = x509.NewCertPool()
	s.pool.AddCert(s.srv.Certificate())
	return s
}

func (s *testStore) requests() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*http.Request(nil), s.reqs...)
}

// serveBlob serves data the way the real store's blob route does.
func serveBlob(data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("X-Image-SHA256", hexSum(data))
		_, _ = w.Write(data)
	}
}

func newFetchFixture(t *testing.T, s *testStore) *imgFixture {
	t.Helper()
	f := newImgFixture(t, config.RoleHypervisor)
	if s != nil {
		f.h.fetchURL = s.srv.URL
		f.h.fetchRoots = s.pool
	}
	return f
}

func fetchBody(sha string, size int, ticket string) string {
	b, _ := json.Marshal(map[string]any{"sha256": sha, "size": size, "ticket": ticket})
	return string(b)
}

func (f *imgFixture) fetch(name, body string) *httptest.ResponseRecorder {
	return fetchVia(f.srv, "hv1", name, body, "")
}

func fetchVia(h http.Handler, host, name, body, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/hosts/"+host+"/images/"+name+"/fetch", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// events parses an NDJSON response: every line is one JSON object.
func events(t *testing.T, rec *httptest.ResponseRecorder) []api.FetchImageEvent {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasSuffix(body, "\n") {
		t.Fatalf("stream does not end in a newline: %q", body)
	}
	var out []api.FetchImageEvent
	for _, l := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		var e api.FetchImageEvent
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		out = append(out, e)
	}
	return out
}

// last is the stream's terminal event, which must be the only done or error.
func last(t *testing.T, evs []api.FetchImageEvent) api.FetchImageEvent {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	for _, e := range evs[:len(evs)-1] {
		if e.Event != api.FetchImageEventEventProgress {
			t.Errorf("non-terminal event %+v", e)
		}
	}
	return evs[len(evs)-1]
}

// nothingStored fails if name left a file, a temporary file or a manifest
// entry.
func (f *imgFixture) nothingStored(t *testing.T, name string) {
	t.Helper()
	f.noPart(t, name)
	if _, err := os.Lstat(filepath.Join(f.pm.dirs[vmtool.HypervisorPool], name)); !os.IsNotExist(err) {
		t.Errorf("%s exists (%v)", name, err)
	}
	if b := f.manifestBytes(t); strings.Contains(string(b), name) {
		t.Errorf("manifest lists %s: %s", name, b)
	}
}

func TestFetchImageStores(t *testing.T) {
	data := randomBytes(t, 300000)
	s := newTestStore(t, serveBlob(data))
	f := newFetchFixture(t, s)
	rec := f.fetch("base.qcow2", fetchBody(hexSum(data), len(data), fetchTicket))
	evs := events(t, rec)
	end := last(t, evs)
	if end.Event != api.FetchImageEventEventDone || end.Image == nil || end.Image.Name != "base.qcow2" ||
		end.Image.Sha256 == nil || *end.Image.Sha256 != hexSum(data) || end.Image.Size == nil || *end.Image.Size != int64(len(data)) {
		t.Fatalf("terminal %+v", end)
	}
	var sawProgress bool
	for _, e := range evs[:len(evs)-1] {
		if e.Received == nil || e.Total == nil || *e.Total != int64(len(data)) || *e.Received > *e.Total {
			t.Errorf("progress %+v", e)
		}
		sawProgress = true
	}
	if !sawProgress {
		t.Error("no progress line")
	}
	got, err := os.ReadFile(filepath.Join(f.pm.dirs[vmtool.HypervisorPool], "base.qcow2"))
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("stored file: %v", err)
	}
	f.noPart(t, "base.qcow2")
	if e := getManifest(t, f.srv, "hv1", "base.qcow2"); e.Sha256 == nil || *e.Sha256 != hexSum(data) {
		t.Errorf("manifest %+v", e)
	}
	if len(f.pm.refreshes) == 0 {
		t.Error("pool not refreshed")
	}
	rs := s.requests()
	if len(rs) != 1 || rs[0].Method != "GET" || rs[0].URL.Path != "/v1/images/base.qcow2/blob" ||
		rs[0].Header.Get("Authorization") != "Ticket "+fetchTicket {
		t.Fatalf("store saw %+v", rs)
	}
}

func TestFetchImageNoStoreURL(t *testing.T) {
	f := newFetchFixture(t, nil)
	rec := f.fetch("base.qcow2", fetchBody(hexSum([]byte("x")), 1, fetchTicket))
	if rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got, want := errText(t, rec), "no store configured on hv1; redeploy with a store"; got != want {
		t.Errorf("message %q, want %q", got, want)
	}
	if !strings.HasPrefix(errText(t, rec), remote.NoStorePrefix) {
		t.Error("message lacks the stable prefix")
	}
	f.nothingStored(t, "base.qcow2")
}

// A digest mismatch (a store whose bytes are not the ones named) leaves no
// file, no temporary file and no manifest entry.
func TestFetchImageDigestMismatch(t *testing.T) {
	want := randomBytes(t, 5000)
	other := randomBytes(t, 5000)
	h := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(other)))
		w.Header().Set("X-Image-SHA256", hexSum(want)) // a lying store
		_, _ = w.Write(other)
	}
	f := newFetchFixture(t, newTestStore(t, h))
	rec := f.fetch("base.qcow2", fetchBody(hexSum(want), len(want), fetchTicket))
	end := last(t, events(t, rec))
	if end.Event != api.FetchImageEventEventError || end.Reason == nil || *end.Reason != api.Mismatch || !strings.Contains(errOf(end), "SHA-256") {
		t.Fatalf("terminal %+v", end)
	}
	f.nothingStored(t, "base.qcow2")
}

// A size mismatch is caught from the store's Content-Length before a byte of
// the body is read; the other mismatching header (X-Image-SHA256) likewise.
func TestFetchImageHeaderMismatchBeforeBody(t *testing.T) {
	data := randomBytes(t, 4000)
	for _, c := range []struct {
		name         string
		size         int
		headerSHA    string
		wantContains string
	}{
		{"size", len(data) + 7, hexSum(data), "the store's image is "},
		{"sha", len(data), hexSum([]byte("other")), "the store's image has SHA-256"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.Header().Set("X-Image-SHA256", c.headerSHA)
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				_, _ = w.Write(data)
			}
			s := newTestStore(t, h)
			f := newFetchFixture(t, s)
			sha := hexSum(data)
			rec := f.fetch("base.qcow2", fetchBody(sha, c.size, fetchTicket))
			end := last(t, events(t, rec))
			if end.Event != api.FetchImageEventEventError || end.Reason == nil || *end.Reason != api.Mismatch || !strings.Contains(errOf(end), c.wantContains) {
				t.Fatalf("terminal %+v", end)
			}
			f.nothingStored(t, "base.qcow2")
		})
	}
}

// A store connection that ends before the body is complete is an `other`
// failure and leaves nothing.
func TestFetchImageShortBody(t *testing.T) {
	data := randomBytes(t, 4000)
	h := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)+100))
		w.Header().Set("X-Image-SHA256", hexSum(data))
		_, _ = w.Write(data)
	}
	f := newFetchFixture(t, newTestStore(t, h))
	rec := f.fetch("base.qcow2", fetchBody(hexSum(data), len(data)+100, fetchTicket))
	end := last(t, events(t, rec))
	if end.Event != api.FetchImageEventEventError || end.Reason == nil || *end.Reason != api.Other {
		t.Fatalf("terminal %+v", end)
	}
	f.nothingStored(t, "base.qcow2")
}

// Already cached with that digest and size: done at once, the store is not
// asked. Cached with another digest: downloaded and replaced.
func TestFetchImageCached(t *testing.T) {
	data := randomBytes(t, 3000)
	s := newTestStore(t, serveBlob(data))
	f := newFetchFixture(t, s)
	f.pm.put(t, vmtool.HypervisorPool, "base.qcow2", data, true)
	f.seedManifest(t, map[string]vmtool.ImageEntry{"base.qcow2": {SHA256: hexSum(data), Size: int64(len(data))}})
	rec := f.fetch("base.qcow2", fetchBody(hexSum(data), len(data), fetchTicket))
	evs := events(t, rec)
	if len(evs) != 1 || evs[0].Event != api.FetchImageEventEventDone || evs[0].Image == nil || evs[0].Image.Name != "base.qcow2" {
		t.Fatalf("events %+v", evs)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("store asked %d times for a cached image", n)
	}

	// An older copy under the same name is replaced.
	f2 := newFetchFixture(t, s)
	old := []byte("old bytes")
	f2.pm.put(t, vmtool.HypervisorPool, "base.qcow2", old, true)
	f2.seedManifest(t, map[string]vmtool.ImageEntry{"base.qcow2": {SHA256: hexSum(old), Size: int64(len(old))}})
	end := last(t, events(t, f2.fetch("base.qcow2", fetchBody(hexSum(data), len(data), fetchTicket))))
	if end.Event != api.FetchImageEventEventDone || len(s.requests()) != 1 {
		t.Fatalf("terminal %+v, store requests %d", end, len(s.requests()))
	}
	if b, _ := os.ReadFile(filepath.Join(f2.pm.dirs[vmtool.HypervisorPool], "base.qcow2")); !bytes.Equal(b, data) {
		t.Error("old copy not replaced")
	}
}

// A foreign name is 409 before the store is contacted; nothing is touched.
func TestFetchImageForeign(t *testing.T) {
	data := []byte("x")
	s := newTestStore(t, serveBlob(data))
	f := newFetchFixture(t, s)
	p := f.pm.put(t, vmtool.HypervisorPool, "base.qcow2", []byte("theirs"), false)
	rec := f.fetch("base.qcow2", fetchBody(hexSum(data), 1, fetchTicket))
	if rec.Code != http.StatusConflict || !strings.Contains(errText(t, rec), "will not replace it") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("store contacted %d times", n)
	}
	if b, _ := os.ReadFile(p); string(b) != "theirs" {
		t.Error("foreign file changed")
	}
}

// One fetch or upload of a name at a time: while a fetch runs, a second fetch
// and an upload of the name are 409 before anything is downloaded or read.
func TestFetchImageOneAtATime(t *testing.T) {
	data := randomBytes(t, 20000)
	reached, release := make(chan struct{}), make(chan struct{})
	h := func(w http.ResponseWriter, r *http.Request) {
		close(reached)
		<-release
		serveBlob(data)(w, r)
	}
	s := newTestStore(t, h)
	f := newFetchFixture(t, s)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- f.fetch("base.qcow2", fetchBody(hexSum(data), len(data), fetchTicket)) }()
	<-reached

	rec := f.fetch("base.qcow2", fetchBody(hexSum(data), len(data), fetchTicket))
	if rec.Code != http.StatusConflict || !strings.Contains(errText(t, rec), "already running") {
		t.Errorf("second fetch: %d %s", rec.Code, rec.Body)
	}
	if rec := f.put("base.qcow2", tripReader{t}, 10, hexSum([]byte("x"))); rec.Code != http.StatusConflict {
		t.Errorf("upload during fetch: %d %s", rec.Code, rec.Body)
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("store asked %d times", n)
	}
	close(release)
	if end := last(t, events(t, <-first)); end.Event != api.FetchImageEventEventDone {
		t.Errorf("first fetch: %+v", end)
	}
	f.noPart(t, "base.qcow2")
}

// The daemon dials only store_url: no request field (or path trick) changes
// scheme, host, port or path prefix, and the store sees the fixed path.
func TestFetchImageDialsOnlyStoreURL(t *testing.T) {
	data := randomBytes(t, 1000)
	var seen []string
	var host string
	s := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.EscapedPath())
		host = r.Host
		serveBlob(data)(w, r)
	})
	decoy := newTestStore(t, serveBlob(data))
	f := newFetchFixture(t, s)
	f.h.fetchURL = s.srv.URL + "/pfx/"

	body, _ := json.Marshal(map[string]any{
		"sha256": hexSum(data), "size": len(data), "ticket": fetchTicket,
		"url": decoy.srv.URL, "store_url": decoy.srv.URL, "host": decoy.srv.URL, "address": decoy.srv.URL,
	})
	if end := last(t, events(t, f.fetch("base.qcow2", string(body)))); end.Event != api.FetchImageEventEventDone {
		t.Fatalf("terminal %+v", end)
	}
	if len(seen) != 1 || seen[0] != "/pfx/v1/images/base.qcow2/blob" {
		t.Errorf("store path %v", seen)
	}
	if host != strings.TrimPrefix(s.srv.URL, "https://") {
		t.Errorf("store saw Host %q", host)
	}
	if n := len(decoy.requests()); n != 0 {
		t.Errorf("a host named in the request was dialled %d times", n)
	}

	// A name that is not a plain file name never reaches the store.
	for _, bad := range []string{"..%2Fetc%2Fpasswd", "a%2Fb", ".hidden", "a%3Fb"} {
		rec := f.fetch(bad, fetchBody(hexSum(data), len(data), fetchTicket))
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound && rec.Code != http.StatusMovedPermanently {
			t.Errorf("name %q: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if len(seen) != 1 {
		t.Errorf("a bad name reached the store: %v", seen)
	}
}

// A redirect is never followed: the target is not contacted and the answer is
// a store_unreachable error line.
func TestFetchImageRedirectNotFollowed(t *testing.T) {
	decoy := newTestStore(t, serveBlob([]byte("decoy")))
	s := newTestStore(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, decoy.srv.URL+"/v1/images/base.qcow2/blob", http.StatusFound)
	})
	f := newFetchFixture(t, s)
	data := []byte("decoy")
	end := last(t, events(t, f.fetch("base.qcow2", fetchBody(hexSum(data), len(data), fetchTicket))))
	if end.Event != api.FetchImageEventEventError || end.Reason == nil || *end.Reason != api.StoreRefused || !strings.Contains(errOf(end), "redirect") {
		t.Fatalf("terminal %+v", end)
	}
	if n := len(decoy.requests()); n != 0 {
		t.Errorf("redirect target contacted %d times (ticket would have gone with it)", n)
	}
	f.nothingStored(t, "base.qcow2")
}

func TestFetchImageStoreRefused(t *testing.T) {
	s := newTestStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
	f := newFetchFixture(t, s)
	end := last(t, events(t, f.fetch("base.qcow2", fetchBody(hexSum([]byte("x")), 1, fetchTicket))))
	if end.Event != api.FetchImageEventEventError || end.Reason == nil || *end.Reason != api.StoreRefused || !strings.Contains(errOf(end), "401") {
		t.Fatalf("terminal %+v", end)
	}
	f.nothingStored(t, "base.qcow2")
}

// An unreachable store, and one whose certificate the system trust store
// does not accept (the daemon's roots are the system's unless a test says
// otherwise), are store_unreachable with the stable phrase.
func TestFetchImageStoreUnreachable(t *testing.T) {
	closed := newTestStore(t, serveBlob([]byte("x")))
	closed.srv.Close()
	f := newFetchFixture(t, closed)
	end := last(t, events(t, f.fetch("base.qcow2", fetchBody(hexSum([]byte("x")), 1, fetchTicket))))
	if end.Event != api.FetchImageEventEventError || end.Reason == nil || *end.Reason != api.StoreUnreachable ||
		!strings.HasPrefix(errOf(end), remote.StoreUnreachablePrefix+"hv1: ") {
		t.Fatalf("terminal %+v", end)
	}
	f.nothingStored(t, "base.qcow2")

	// System trust store (no injected CA): the test store's certificate is
	// self-signed, so TLS fails.
	s := newTestStore(t, serveBlob([]byte("x")))
	f2 := newFetchFixture(t, s)
	f2.h.fetchRoots = nil
	end = last(t, events(t, f2.fetch("base.qcow2", fetchBody(hexSum([]byte("x")), 1, fetchTicket))))
	if end.Event != api.FetchImageEventEventError || end.Reason == nil || *end.Reason != api.StoreUnreachable {
		t.Fatalf("untrusted store: %+v", end)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("an untrusted store got %d requests", n)
	}
}

func TestFetchImageBadRequests(t *testing.T) {
	data := []byte("x")
	s := newTestStore(t, serveBlob(data))
	f := newFetchFixture(t, s)
	sha := hexSum(data)
	for name, body := range map[string]string{
		"not json":       "{",
		"empty":          "",
		"no ticket":      fmt.Sprintf(`{"sha256":%q,"size":1}`, sha),
		"short ticket":   fetchBody(sha, 1, "abc"),
		"ticket chars":   fetchBody(sha, 1, fetchTicket+" x"),
		"ticket newline": fetchBody(sha, 1, fetchTicket+"\r\nX-Evil: 1"),
		"bad sha":        fetchBody("zz", 1, fetchTicket),
		"zero size":      fetchBody(sha, 0, fetchTicket),
		"negative size":  fetchBody(sha, -1, fetchTicket),
	} {
		rec := f.fetch("base.qcow2", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "PLACEHOLDER-not-a-secret") {
			t.Errorf("%s: ticket echoed: %s", name, rec.Body)
		}
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("store asked %d times", n)
	}
}

// A workstation keeps no image cache: its own unlisted name is 403, and the
// store is not asked.
func TestFetchImageWorkstationRefusesOwn(t *testing.T) {
	s := newTestStore(t, serveBlob([]byte("x")))
	f := newImgFixture(t, config.RoleWorkstation)
	f.h.fetchURL, f.h.fetchRoots = s.srv.URL, s.pool
	rec := f.fetch("base.qcow2", fetchBody(hexSum([]byte("x")), 1, fetchTicket))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("store asked %d times", n)
	}
}

// A workstation forwards fetchImage to a listed host's daemon and relays its
// NDJSON; the ticket goes to the daemon only in the body and to the store only
// in the Authorization header.
func TestFetchImageWorkstationForwards(t *testing.T) {
	data := randomBytes(t, 50000)
	s := newTestStore(t, serveBlob(data))
	hv := newFetchFixture(t, s)
	hvSrv := httptest.NewTLSServer(hv.srv)
	t.Cleanup(hvSrv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(hvSrv.Certificate())

	var logs lockedBuf
	restore := captureLogs(&logs)
	defer restore()

	wsh := &handlers{role: config.RoleWorkstation}
	wsh.hosts = hosts.NewWith("ws", config.RoleWorkstation, []string{"hv1"}, func() (app.Manager, func(), error) {
		t.Error("workstation libvirt opened")
		return nil, nil, errors.New("no libvirt")
	}).WithRemote(func(host, tok string) (app.Manager, func(), error) {
		m, err := remote.New(host, hvSrv.URL, tok, remote.WithRootCAs(pool))
		if err != nil {
			return nil, nil, err
		}
		return m, m.Close, nil
	})
	ws := handlerFor(wsh)
	rec := fetchVia(ws, "hv1", "base.qcow2", fetchBody(hexSum(data), len(data), fetchTicket), "caller-token")
	end := last(t, events(t, rec))
	if end.Event != api.FetchImageEventEventDone {
		t.Fatalf("terminal %+v: %s", end, rec.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(hv.pm.dirs[vmtool.HypervisorPool], "base.qcow2")); !bytes.Equal(b, data) {
		t.Error("image not stored on the hypervisor")
	}
	if rs := s.requests(); len(rs) != 1 || rs[0].Header.Get("Authorization") != "Ticket "+fetchTicket {
		t.Errorf("store saw %+v", rs)
	}
	if strings.Contains(rec.Body.String(), fetchTicket) || strings.Contains(logs.String(), fetchTicket) {
		t.Error("ticket in the workstation's response or log")
	}

	// A refusal before the stream (foreign) is a plain JSON status.
	hv.pm.put(t, vmtool.HypervisorPool, "mine.qcow2", []byte("theirs"), false)
	rec = fetchVia(ws, "hv1", "mine.qcow2", fetchBody(hexSum(data), len(data), fetchTicket), "caller-token")
	if rec.Code != http.StatusConflict || !strings.Contains(errText(t, rec), "will not replace it") {
		t.Errorf("foreign via workstation: %d %s", rec.Code, rec.Body)
	}
	// An error line is relayed as an error line.
	s.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }
	rec = fetchVia(ws, "hv1", "other.qcow2", fetchBody(hexSum(data), len(data), fetchTicket), "caller-token")
	end = last(t, events(t, rec))
	if end.Event != api.FetchImageEventEventError || end.Reason == nil || *end.Reason != api.StoreRefused {
		t.Errorf("relayed error %+v", end)
	}
}

// --- the capture test ---

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLogs(w io.Writer) (restore func()) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }
}

// The ticket reaches nothing but the store's Authorization header: not a log
// line (debug level), not a response body or header, not an NDJSON line, not
// an error the handler builds; across success, both mismatches, a refusing
// store, an unreachable one, a redirect, no store_url and a malformed ticket.
func TestFetchImageTicketNeverLeaks(t *testing.T) {
	data := randomBytes(t, 20000)
	sha := hexSum(data)
	var logs lockedBuf
	defer captureLogs(&logs)()

	type scenario struct {
		name  string
		store http.HandlerFunc // nil: no store_url configured
		body  string
	}
	redirTarget := newTestStore(t, serveBlob(data))
	scenarios := []scenario{
		{"success", serveBlob(data), fetchBody(sha, len(data), fetchTicket)},
		{"digest mismatch", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			_, _ = w.Write(randomBytes(t, len(data)))
		}, fetchBody(sha, len(data), fetchTicket)},
		{"size mismatch", serveBlob(data), fetchBody(sha, len(data)+1, fetchTicket)},
		{"store 401", func(w http.ResponseWriter, r *http.Request) {
			// A hostile store echoes what it was sent.
			http.Error(w, `{"error":"bad `+r.Header.Get("Authorization")+`"}`, http.StatusUnauthorized)
		}, fetchBody(sha, len(data), fetchTicket)},
		{"store unreachable", nil, fetchBody(sha, len(data), fetchTicket)},
		{"redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, redirTarget.srv.URL+"/?t="+fetchTicket, http.StatusFound)
		}, fetchBody(sha, len(data), fetchTicket)},
		{"no store_url", nil, fetchBody(sha, len(data), fetchTicket)},
		{"malformed ticket", serveBlob(data), fetchBody(sha, len(data), fetchTicket+" "+fetchTicket)},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			var f *imgFixture
			switch {
			case sc.name == "no store_url":
				f = newFetchFixture(t, nil)
			case sc.store == nil: // unreachable
				dead := newTestStore(t, serveBlob(data))
				dead.srv.Close()
				f = newFetchFixture(t, dead)
			default:
				f = newFetchFixture(t, newTestStore(t, sc.store))
			}
			rec := f.fetch("base.qcow2", sc.body)
			all := rec.Body.String()
			for k, vs := range rec.Header() {
				all += k + ": " + strings.Join(vs, ",") + "\n"
			}
			if strings.Contains(all, "PLACEHOLDER-not-a-secret") {
				t.Errorf("ticket in the response: %s", all)
			}
			// The error the handler builds before it writes a line.
			if f.h.fetchURL != "" {
				var req api.FetchImageRequest
				_ = json.Unmarshal([]byte(sc.body), &req)
				resp, fail := f.h.openBlob(context.Background(), "hv1", "base.qcow2", req)
				if resp != nil {
					resp.Body.Close()
				}
				if fail != nil && strings.Contains(fail.msg, "PLACEHOLDER-not-a-secret") {
					t.Errorf("ticket in the failure message: %s", fail.msg)
				}
			}
		})
	}
	if strings.Contains(logs.String(), "PLACEHOLDER-not-a-secret") {
		t.Errorf("ticket in the log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "fetchImage") {
		t.Errorf("the capture saw no fetchImage log line, so it proves nothing:\n%s", logs.String())
	}
	// Nor is the request body logged.
	if strings.Contains(logs.String(), `"ticket"`) {
		t.Errorf("request body in the log:\n%s", logs.String())
	}
}

// fetchFailure messages are built from the store URL and a cause, never from
// a header: a transport error that would name the Authorization value is
// scrubbed.
func TestFetchFailureScrubsTicket(t *testing.T) {
	got := scrubTicket(errors.New("weird "+fetchTicket+" error").Error(), fetchTicket)
	if strings.Contains(got, fetchTicket) {
		t.Errorf("not scrubbed: %q", got)
	}
}

func errOf(e api.FetchImageEvent) string {
	if e.Error == nil {
		return ""
	}
	return *e.Error
}
