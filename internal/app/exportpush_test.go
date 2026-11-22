package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/storeclient"
)

// `templates export --push`: the workflow. The host is a fake (xferHost); the
// store is an in-process TLS double that, like the real one, checks the
// pushed bytes against X-Image-SHA256 and Content-Length.

type xpStore struct {
	ts       *httptest.Server
	mu       sync.Mutex
	images   map[string][]byte
	puts     int
	whoamis  int
	failBody string // when set, a failing PUT answers 500 with this body
	headers  []http.Header
	urls     []string
}

func newXPStore(t *testing.T) *xpStore {
	t.Helper()
	f := &xpStore{images: map[string][]byte{}}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.headers = append(f.headers, r.Header.Clone())
		f.urls = append(f.urls, r.URL.String())
		if r.Header.Get("Authorization") != "Bearer "+stTok {
			out(401, map[string]string{"error": "unauthorized"})
			return
		}
		if r.URL.Path == "/v1/whoami" {
			f.whoamis++
			out(200, map[string]string{"name": "jared"})
			return
		}
		name, ok := strings.CutPrefix(r.URL.Path, "/v1/images/")
		if !ok || r.Method != http.MethodPut {
			out(404, map[string]string{"error": "not found"})
			return
		}
		f.puts++
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])
		if f.failBody != "" {
			// A hostile store: it echoes the request's headers in its error.
			out(500, map[string]any{"error": f.failBody + " " + r.Header.Get("Authorization")})
			return
		}
		if got != r.Header.Get("X-Image-SHA256") || int64(len(body)) != r.ContentLength {
			out(400, map[string]string{"error": "digest or size mismatch"})
			return
		}
		f.images[name] = body
		out(201, map[string]any{"name": name, "sha256": got, "size": len(body), "updated": time.Now()})
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *xpStore) svc(t *testing.T, notes *[]string) *Service {
	t.Helper()
	pool := f.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	return &Service{Store: OpenStoreSession(f.ts.URL, func() (string, error) { return stTok, nil },
		[]storeclient.Option{storeclient.WithRootCAs(pool)}, func(l string) {
			if notes != nil {
				*notes = append(*notes, l)
			}
		})}
}

func (f *xpStore) get(name string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.images[name]
}

func xpHost(blob []byte) *xferHost {
	return &xferHost{
		events:   []ExportEvent{{Stage: "export", Received: 1, Total: int64(len(blob))}},
		exported: ExportedImage{Name: "proj", SHA256: sumHex(blob), Size: int64(len(blob)), Path: "/var/lib/vmtool/images/proj"},
		blob:     blob,
	}
}

func TestExportTemplateToStoreStreamsTheBlobIntoTheStore(t *testing.T) {
	blob := []byte(strings.Repeat("PLACEHOLDER image bytes ", 1000))
	st := newXPStore(t)
	h := xpHost(blob)
	var evs []ProgressEvent
	res, err := st.svc(t, nil).ExportTemplateToStore(context.Background(), h,
		ExportTemplateInput{Host: "hv1", Name: "proj", Pool: "thin-test", OnProgress: func(e ProgressEvent) { evs = append(evs, e) }})
	if err != nil {
		t.Fatal(err)
	}
	if string(st.get("proj")) != string(blob) {
		t.Fatalf("the store holds %d bytes, want %d", len(st.get("proj")), len(blob))
	}
	if res.Store.Name != "proj" || res.Store.SHA256 != sumHex(blob) || res.Store.Size != int64(len(blob)) || res.Image.Name != "proj" {
		t.Errorf("result %+v", res)
	}
	if strings.Join(h.exports, "|") != "proj thin-test" || h.opens != 1 {
		t.Errorf("exports %v opens %d", h.exports, h.opens)
	}
	var stages []string
	var pushDone, pushProg bool
	for _, e := range evs {
		stages = append(stages, e.Stage+"/"+e.Status)
		if e.Stage == "push" && e.Status == "progress" && e.Total == int64(len(blob)) {
			pushProg = true
		}
		if e.Stage == "push" && e.Status == "done" {
			pushDone = true
		}
	}
	if !pushProg || !pushDone || stages[0] != "export/start" {
		t.Errorf("events %v", stages)
	}
}

// The store, not this machine, decides: a digest mismatch (corrupt bytes
// under the daemon's true digest, or a wrong header over true bytes) is an
// error and nothing is claimed pushed.
func TestExportTemplateToStoreStoreRejectsADigestMismatch(t *testing.T) {
	blob := []byte(strings.Repeat("PLACEHOLDER image bytes ", 100))
	corrupt := append([]byte{blob[0] ^ 0xff}, blob[1:]...)
	for name, h := range map[string]*xferHost{
		"corrupt bytes": func() *xferHost { h := xpHost(blob); h.blob = corrupt; h.digest = sumHex(blob); return h }(),
		"wrong digest": func() *xferHost {
			h := xpHost(blob)
			h.digest = strings.Repeat("a", 64)
			h.exported.SHA256 = h.digest
			return h
		}(),
	} {
		st := newXPStore(t)
		var evs []ProgressEvent
		res, err := st.svc(t, nil).ExportTemplateToStore(context.Background(), h,
			ExportTemplateInput{Host: "hv1", Name: "proj", OnProgress: func(e ProgressEvent) { evs = append(evs, e) }})
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		var ae *Error
		if !AsError(err, &ae) || ae.Kind != KindBadGateway {
			t.Errorf("%s: kind of %v", name, err)
		}
		if st.get("proj") != nil || res.Store.Name != "" {
			t.Errorf("%s: claimed pushed: %+v", name, res)
		}
		for _, e := range evs {
			if e.Stage == "push" && e.Status == "done" {
				t.Errorf("%s: a push done event: %+v", name, e)
			}
		}
		if st.puts != 1 {
			t.Errorf("%s: %d PUTs", name, st.puts)
		}
	}
}

// The export's done event and the blob's headers must agree: a blob that is
// not the image the export reported is not pushed.
func TestExportTemplateToStoreRefusesABlobThatIsNotTheExportedImage(t *testing.T) {
	blob := []byte("PLACEHOLDER image bytes")
	st := newXPStore(t)
	h := xpHost(blob)
	h.exported.SHA256 = strings.Repeat("b", 64)
	if _, err := st.svc(t, nil).ExportTemplateToStore(context.Background(), h, ExportTemplateInput{Host: "hv1", Name: "proj"}); err == nil {
		t.Fatal("pushed a blob that is not the exported image")
	}
	if st.puts != 0 {
		t.Errorf("%d PUTs", st.puts)
	}
}

// Local mode (no store, a store that is down): refused before the daemon is
// contacted, naming the way to do it without a store.
func TestExportTemplateToStoreNeedsAStore(t *testing.T) {
	blob := []byte("PLACEHOLDER image bytes")
	check := func(name string, s *Service, h *xferHost) {
		t.Helper()
		_, err := s.ExportTemplateToStore(context.Background(), h, ExportTemplateInput{Host: "hv1", Name: "proj"})
		var ae *Error
		if !AsError(err, &ae) || ae.Kind != KindInvalid || !errors.Is(err, ErrNoStore) ||
			!strings.Contains(err.Error(), "vmtool images pull hv1/proj") || !strings.Contains(err.Error(), "images push") {
			t.Errorf("%s: %v", name, err)
		}
		if len(h.exports) != 0 || h.opens != 0 {
			t.Errorf("%s: the daemon was contacted: %v %d", name, h.exports, h.opens)
		}
	}
	check("no server", &Service{}, xpHost(blob))
	st := newXPStore(t)
	svc := st.svc(t, nil)
	st.ts.Close() // a configured store that does not answer
	check("store down", svc, xpHost(blob))
}

func TestExportTemplateToStoreDaemonFailuresPushNothing(t *testing.T) {
	blob := []byte("PLACEHOLDER image bytes")
	ctx := context.Background()
	in := ExportTemplateInput{Host: "hv1", Name: "proj"}
	for name, h := range map[string]*xferHost{
		"export fails": func() *xferHost { h := xpHost(blob); h.exportErr = errors.New("no space"); return h }(),
		"blob refused": func() *xferHost {
			h := xpHost(blob)
			h.openErr = &Error{Kind: KindNotFound, Err: errors.New("not a cached image")}
			return h
		}(),
		"no digest": func() *xferHost { h := xpHost(blob); h.digest = "nope"; return h }(),
		"no size":   func() *xferHost { h := xpHost(blob); h.declared = -1; return h }(),
	} {
		st := newXPStore(t)
		if _, err := st.svc(t, nil).ExportTemplateToStore(ctx, h, in); err == nil {
			t.Errorf("%s: no error", name)
		}
		if st.puts != 0 {
			t.Errorf("%s: %d PUTs", name, st.puts)
		}
	}
	// Bad names and hosts with no daemon never reach the store or the host.
	st := newXPStore(t)
	if _, err := st.svc(t, nil).ExportTemplateToStore(ctx, struct{ Manager }{}, in); err == nil {
		t.Error("a host without a daemon")
	}
	if _, err := st.svc(t, nil).ExportTemplateToStore(ctx, xpHost(blob), ExportTemplateInput{Host: "hv1", Name: "../x"}); err == nil {
		t.Error("bad name")
	}
	if st.puts != 0 || st.whoamis != 0 {
		t.Errorf("store touched: %d puts %d whoamis", st.puts, st.whoamis)
	}
}

// The store token goes to the store only; a hostile store that echoes it in an
// error body does not get it into the error, the progress or the notes.
func TestExportTemplateToStoreNeverShowsTheToken(t *testing.T) {
	blob := []byte("PLACEHOLDER image bytes")
	st := newXPStore(t)
	st.failBody = "boom"
	var notes []string
	var evs []ProgressEvent
	_, err := st.svc(t, &notes).ExportTemplateToStore(context.Background(), xpHost(blob),
		ExportTemplateInput{Host: "hv1", Name: "proj", OnProgress: func(e ProgressEvent) { evs = append(evs, e) }})
	if err == nil {
		t.Fatal("no error")
	}
	all := err.Error() + strings.Join(notes, "|")
	for _, e := range evs {
		all += e.Stage + e.Status + e.Detail + e.Output
	}
	if strings.Contains(all, stTok) || strings.Contains(all, "Bearer") {
		t.Errorf("token in %q", all)
	}
}

// --push has the host hash the export first (hash=true): the store wants the
// digest before the bytes. A plain export does not ask.
func TestExportTemplateToStoreAsksTheHostToHash(t *testing.T) {
	blob := []byte(strings.Repeat("PLACEHOLDER image bytes ", 1000))
	st := newXPStore(t)
	h := xpHost(blob)
	if _, err := st.svc(t, nil).ExportTemplateToStore(context.Background(), h, ExportTemplateInput{Host: "hv1", Name: "proj"}); err != nil {
		t.Fatal(err)
	}
	if len(h.hashes) != 1 || !h.hashes[0] {
		t.Errorf("export asked with hash=%v, want true", h.hashes)
	}
	h2 := xpHost(blob)
	if _, err := (&Service{}).ExportTemplate(context.Background(), h2, ExportTemplateInput{Host: "hv1", Name: "proj"}); err != nil {
		t.Fatal(err)
	}
	if len(h2.hashes) != 1 || h2.hashes[0] {
		t.Errorf("a plain export asked with hash=%v, want false", h2.hashes)
	}
}
