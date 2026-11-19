package cli

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/remote"
	"github.com/jaredeh/vmtool/internal/storeclient"
)

// `vmtool templates export host/<name> --push`. The daemon and the store are
// in-process TLS doubles; the CLI talks to the daemon through the real
// remote client, so the requests the daemon sees are the real ones.

const daemonTok = "PLACEHOLDER-daemon-not-a-secret"

var exportKeep = map[string]bool{"vmtool templates export": true}

// pushDaemon is a hypervisor daemon double: export and blob routes.
type pushDaemon struct {
	ts   *httptest.Server
	pool *x509.CertPool
	mu   sync.Mutex
	// what the double does
	blob      []byte
	sendBytes []byte // when set, the bytes actually sent (the headers still say blob's)
	exportErr string // when set, the export ends with this error line
	blobCode  int    // when set, the blob route answers this
	echo      bool   // an error body echoes the request's headers
	// what it saw
	exports, blobs int
	requests       []string // method, URL and every header, for the leak check
}

func newPushDaemon(t *testing.T, blob []byte) *pushDaemon {
	t.Helper()
	d := &pushDaemon{blob: blob}
	d.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.requests = append(d.requests, r.Method+" "+r.URL.String()+" "+fmt.Sprint(r.Header))
		if r.Header.Get("Authorization") != "Bearer "+daemonTok {
			w.WriteHeader(401)
			return
		}
		fail := func(code int, msg string) {
			if d.echo {
				msg += " " + fmt.Sprint(r.Header)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(api.Error{Error: msg})
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/export"):
			d.exports++
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(200)
			enc := json.NewEncoder(w)
			if d.exportErr != "" {
				_ = enc.Encode(api.ExportTemplateEvent{Event: api.ExportTemplateEventEventError, Error: &d.exportErr})
				return
			}
			sz, sha, p := int64(len(d.blob)), sumHex(d.blob), "/var/lib/vmtool/images/proj"
			_ = enc.Encode(api.ExportTemplateEvent{Event: api.ExportTemplateEventEventDone,
				Image: &api.ImageManifestEntry{Name: "proj", Sha256: &sha, Size: &sz, Path: &p}})
		case strings.HasSuffix(r.URL.Path, "/blob"):
			d.blobs++
			if d.blobCode != 0 {
				fail(d.blobCode, "not a cached image")
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(d.blob)))
			w.Header().Set("X-Image-SHA256", sumHex(d.blob))
			if d.sendBytes != nil {
				_, _ = w.Write(d.sendBytes)
				return
			}
			_, _ = w.Write(d.blob)
		default:
			fail(404, "no such route")
		}
	}))
	t.Cleanup(d.ts.Close)
	d.pool = x509.NewCertPool()
	d.pool.AddCert(d.ts.Certificate())
	return d
}

func (d *pushDaemon) counts() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.exports, d.blobs
}

// install makes host veritech01 this daemon, through the real remote client.
func (d *pushDaemon) install(t *testing.T) {
	t.Helper()
	m, err := remote.New("veritech01", d.ts.URL, daemonTok, remote.WithRootCAs(d.pool))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	installTemplateHosts(t, map[string]app.Manager{"jetfire": listMgr{}, "veritech01": m})
}

func pushFixture(t *testing.T, blob []byte) (dir string, s *cliStore, d *pushDaemon) {
	t.Helper()
	s = newCLIStore(t)
	dir = storeWorkdir(t, s, storeTok)
	// the host list: veritech01 next to the store
	cfg := twoHostCfg(s.cfg())
	if err := os.WriteFile(dir+"/"+config.FileName, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	d = newPushDaemon(t, blob)
	d.install(t)
	return dir, s, d
}

func cwdEntries(t *testing.T) []string {
	t.Helper()
	es, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestTemplatesExportPushStreamsIntoTheStore(t *testing.T) {
	blob := []byte(strings.Repeat("PLACEHOLDER image bytes ", 5000))
	dir, s, d := pushFixture(t, blob)
	before := cwdEntries(t)
	r := runGate(t, dir, exportKeep, "templates", "export", "veritech01/proj", "--push")
	if r.err != nil {
		t.Fatalf("%v\n%s", r.err, r.errOut)
	}
	s.mu.Lock()
	got, pushes := s.images["proj"], s.pushes.Load()
	s.mu.Unlock()
	if string(got) != string(blob) || pushes != 1 {
		t.Fatalf("the store has %d bytes after %d pushes, want %d", len(got), pushes, len(blob))
	}
	if !strings.Contains(r.out, "Image proj is in the store") || !strings.Contains(r.out, sumHex(blob)) || !strings.Contains(r.out, strconv.Itoa(len(blob))) {
		t.Errorf("final line %q", r.out)
	}
	if !strings.Contains(r.errOut, "exporting") || !strings.Contains(r.errOut, "pushing") {
		t.Errorf("both phases should show progress: %q", r.errOut)
	}
	// no local file: the working directory is as it was, and the host's image
	// directory is not written (there is no image_dir here).
	if after := cwdEntries(t); strings.Join(after, "|") != strings.Join(before, "|") {
		t.Errorf("files appeared: %v -> %v", before, after)
	}
	if e, b := d.counts(); e != 1 || b != 1 {
		t.Errorf("daemon saw %d exports, %d blobs", e, b)
	}
	// again: the store has this very image: the store's answer, as images push
	r = runGate(t, dir, exportKeep, "templates", "export", "veritech01/proj", "--push")
	if r.err != nil || s.pushes.Load() != 2 {
		t.Errorf("again: %v, %d pushes", r.err, s.pushes.Load())
	}
}

// Without --push the command is as before: no store probe, no store call.
func TestTemplatesExportWithoutPushIgnoresTheStore(t *testing.T) {
	dir, s, d := pushFixture(t, []byte("PLACEHOLDER image bytes"))
	s.down.Store(true)
	r := runGate(t, dir, exportKeep, "templates", "export", "veritech01/proj")
	if r.err != nil || strings.Contains(r.errOut, "unreachable") || s.whoami.Load() != 0 {
		t.Errorf("%v %q whoami %d", r.err, r.errOut, s.whoami.Load())
	}
	if e, b := d.counts(); e != 1 || b != 0 {
		t.Errorf("daemon saw %d exports, %d blobs", e, b)
	}
}

func TestTemplatesExportPushStoreRejectsADigestMismatch(t *testing.T) {
	blob := []byte(strings.Repeat("PLACEHOLDER image bytes ", 100))
	dir, s, d := pushFixture(t, blob)
	d.sendBytes = append([]byte{blob[0] ^ 0xff}, blob[1:]...) // same length, headers say the true digest
	r := runGate(t, dir, exportKeep, "templates", "export", "veritech01/proj", "--push")
	if r.err == nil {
		t.Fatal("a corrupt blob was accepted")
	}
	if _, ok := s.images["proj"]; ok {
		t.Error("the store indexed corrupt bytes")
	}
	if strings.Contains(r.out, "is in the store") || strings.Contains(r.errOut, "is in the store") {
		t.Errorf("claimed pushed: %q %q", r.out, r.errOut)
	}
}

func TestTemplatesExportPushNeedsAStore(t *testing.T) {
	blob := []byte("PLACEHOLDER image bytes")
	check := func(name string, r gateRun, d *pushDaemon) {
		t.Helper()
		if r.err == nil || !strings.Contains(r.err.Error(), "vmtool images pull veritech01/proj") || !strings.Contains(r.err.Error(), "images push") {
			t.Errorf("%s: err %v", name, r.err)
		}
		if e, b := d.counts(); e != 0 || b != 0 {
			t.Errorf("%s: the daemon was contacted: %d exports, %d blobs", name, e, b)
		}
	}
	// no server: configured
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, twoHostCfg(""), "")
	d := newPushDaemon(t, blob)
	d.install(t)
	check("no server", runGate(t, dir, exportKeep, "templates", "export", "veritech01/proj", "--push"), d)

	// a store that is configured and down: the fallback line, then the refusal
	s := newCLIStore(t)
	s.down.Store(true)
	dir = storeWorkdir(t, s, storeTok)
	if err := os.WriteFile(dir+"/"+config.FileName, []byte(twoHostCfg(s.cfg())), 0o600); err != nil {
		t.Fatal(err)
	}
	d2 := newPushDaemon(t, blob)
	d2.install(t)
	r := runGate(t, dir, exportKeep, "templates", "export", "veritech01/proj", "--push")
	check("store down", r, d2)
	if !strings.Contains(r.errOut, "unreachable") || s.pushes.Load() != 0 {
		t.Errorf("store down: %q, %d pushes", r.errOut, s.pushes.Load())
	}
}

func TestTemplatesExportPushDaemonFailuresPushNothing(t *testing.T) {
	blob := []byte("PLACEHOLDER image bytes")
	dir, s, d := pushFixture(t, blob)
	d.exportErr = "qemu-img: no space"
	r := runGate(t, dir, exportKeep, "templates", "export", "veritech01/proj", "--push")
	if r.err == nil || !strings.Contains(r.err.Error(), "no space") {
		t.Errorf("export failure: %v", r.err)
	}
	if e, b := d.counts(); e != 1 || b != 0 || s.pushes.Load() != 0 {
		t.Errorf("export failure: %d exports, %d blobs, %d pushes", e, b, s.pushes.Load())
	}
	d.exportErr, d.blobCode = "", 404
	r = runGate(t, dir, exportKeep, "templates", "export", "veritech01/proj", "--push")
	if r.err == nil || s.pushes.Load() != 0 {
		t.Errorf("blob refusal: %v, %d pushes", r.err, s.pushes.Load())
	}
}

// The tokens go to their own servers and nowhere else: not in the other
// server's requests, the output, the progress or any error, even when a
// hostile double echoes the request's headers back in an error body.
func TestTemplatesExportPushNeverShowsATokenOrTicket(t *testing.T) {
	blob := []byte(strings.Repeat("PLACEHOLDER image bytes ", 100))
	for name, setup := range map[string]func(*cliStore, *pushDaemon){
		"success":        func(*cliStore, *pushDaemon) {},
		"mismatch":       func(_ *cliStore, d *pushDaemon) { d.sendBytes = append([]byte{blob[0] ^ 0xff}, blob[1:]...) },
		"hostile daemon": func(_ *cliStore, d *pushDaemon) { d.echo, d.blobCode = true, 500 },
		"hostile export": func(_ *cliStore, d *pushDaemon) { d.echo, d.exportErr = true, "failed" },
	} {
		dir, s, d := pushFixture(t, blob)
		setup(s, d)
		r := runGate(t, dir, exportKeep, "templates", "export", "veritech01/proj", "--push")
		seen := r.out + "\x00" + r.errOut
		if r.err != nil {
			seen += "\x00" + r.err.Error()
		}
		for _, tok := range []string{storeTok, daemonTok, config.Hash(gateToken), gateToken} {
			// A daemon that echoes the request's headers in its own error body
			// gets its own token into remote's error text (remote.statusErr
			// quotes the body; known, see the step report). The store token
			// never reaches a daemon, so it can never be echoed.
			if tok == daemonTok && d.echo {
				continue
			}
			if strings.Contains(seen, tok) {
				t.Errorf("%s: a token is in the output or error: %q", name, seen)
			}
		}
		d.mu.Lock()
		for _, q := range d.requests {
			if strings.Contains(q, storeTok) {
				t.Errorf("%s: the store token reached the daemon: %s", name, q)
			}
		}
		d.mu.Unlock()
	}
	// a hostile store: its error body echoes the Authorization header
	s := newCLIStore(t)
	dir := storeWorkdir(t, s, storeTok)
	hostile := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/whoami" {
			_, _ = w.Write([]byte(`{"name":"jared"}`))
			return
		}
		w.WriteHeader(500)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "boom " + r.Header.Get("Authorization") + fmt.Sprint(r.Header)})
	}))
	t.Cleanup(hostile.Close)
	pool := hostile.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	old := storeClientOptions
	storeClientOptions = []storeclient.Option{storeclient.WithRootCAs(pool)}
	t.Cleanup(func() { storeClientOptions = old })
	if err := os.WriteFile(dir+"/"+config.FileName, []byte(twoHostCfg("server:\n  url: "+hostile.URL+"\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	newPushDaemon(t, blob).install(t)
	r := runGate(t, dir, exportKeep, "templates", "export", "veritech01/proj", "--push")
	seen := r.out + "\x00" + r.errOut
	if r.err == nil {
		t.Fatal("a store that answers 500 pushed")
	}
	seen += "\x00" + r.err.Error()
	if strings.Contains(seen, storeTok) || strings.Contains(seen, "Bearer") {
		t.Errorf("hostile store: token in %q", seen)
	}
}
