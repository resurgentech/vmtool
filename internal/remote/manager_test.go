package remote

import (
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// specVersion is this binary's API version, read the way the client reads it.
func specVersion(t *testing.T) string {
	t.Helper()
	sw, err := api.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	return sw.Info.Version
}

// fakeAPI is a TLS daemon stand-in: GET /version answers version, every other
// request goes to h. It records each request's path and Authorization.
type fakeAPI struct {
	srv      *httptest.Server
	pool     *x509.CertPool
	versions atomic.Int32
	mu       sync.Mutex
	paths    []string
	auths    []string
}

func newFakeAPI(t *testing.T, version string, h http.HandlerFunc) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if r.URL.Path == "/version" {
			f.versions.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(api.VersionInfo{ApiVersion: version, BuildVersion: "test", Host: "veritech01"})
			return
		}
		h(w, r)
	}))
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	f.pool = x509.NewCertPool()
	f.pool.AddCert(f.srv.Certificate())
	return f
}

func (f *fakeAPI) seen() (paths, auths []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...), append([]string(nil), f.auths...)
}

func (f *fakeAPI) manager(t *testing.T, opts ...Option) *Manager {
	t.Helper()
	m, err := New("veritech01", f.srv.URL, testToken, append([]Option{WithRootCAs(f.pool)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var _ app.Manager = (*Manager)(nil)

// The client never reads GET /version before an operation and never compares
// versions: a daemon reporting another api_version serves operations normally,
// and every operation carries the bearer token.
func TestNoVersionCheck(t *testing.T) {
	f := newFakeAPI(t, "9.9.9", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			jsonReply(w, 200, []api.VMInfo{})
			return
		}
		w.WriteHeader(204)
	})
	m := f.manager(t)
	if _, err := m.List(); err != nil {
		t.Fatalf("List against api_version 9.9.9: %v", err)
	}
	if err := m.Start("web"); err != nil {
		t.Fatalf("Start against api_version 9.9.9: %v", err)
	}
	if n := f.versions.Load(); n != 0 {
		t.Errorf("GET /version ran %d times, want 0", n)
	}
	paths, auths := f.seen()
	if len(paths) != 2 || paths[0] != "GET /v1/hosts/veritech01/vms" {
		t.Errorf("paths = %v", paths)
	}
	for _, a := range auths {
		if a != "Bearer "+testToken {
			t.Errorf("Authorization = %q", a)
		}
	}
}

// The daemon's status maps onto the app layer's kinds, with the host named
// and the token never in the text.
func TestStatusMapping(t *testing.T) {
	var status atomic.Int32
	var msg atomic.Value
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		s := int(status.Load())
		if s == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
		jsonReply(w, s, api.Error{Error: msg.Load().(string)})
	})
	m := f.manager(t)
	for _, tc := range []struct {
		status   int
		body     string
		kind     app.Kind
		notFound bool
		want     string // exact error text when set, else a substring
		exact    bool
	}{
		{401, "unauthorized: missing or invalid bearer token", app.KindBadGateway, false,
			"token rejected by veritech01; run `just token-fetch` or `just deploy veritech01`", true},
		{403, "createVM is served by the workstation, not by hypervisor veritech01", app.KindBadGateway, false,
			"served by the workstation", false},
		{403, "", app.KindBadGateway, false, "served by the workstation", false},
		{404, `domain "web" not found`, app.KindNotFound, true, `veritech01: domain "web" not found`, true},
		{400, "size_gb must be > 0", app.KindInvalid, false, "veritech01: size_gb must be > 0", true},
		{409, "volume exists", app.KindConflict, false, "veritech01: volume exists", true},
		{500, "libvirt exploded", app.KindInternal, false, "veritech01: libvirt exploded", true},
		{502, "", app.KindBadGateway, false, "502", false},
	} {
		status.Store(int32(tc.status))
		msg.Store(tc.body)
		_, err := m.Info("web")
		if err == nil {
			t.Fatalf("%d: no error", tc.status)
		}
		var ae *app.Error
		if !app.AsError(err, &ae) || ae.Kind != tc.kind {
			t.Errorf("%d: err %v kind %v, want %v", tc.status, err, ae, tc.kind)
		}
		if vmtool.IsNotFound(err) != tc.notFound {
			t.Errorf("%d: IsNotFound = %v", tc.status, !tc.notFound)
		}
		if tc.exact && err.Error() != tc.want {
			t.Errorf("%d: %q\nwant %q", tc.status, err, tc.want)
		}
		if !tc.exact && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%d: %q lacks %q", tc.status, err, tc.want)
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("%d: error contains the token", tc.status)
		}
	}
}

// slowAPI answers every operation after delay (or when the client gives up).
func slowAPI(t *testing.T, delay time.Duration, reply func(w http.ResponseWriter, r *http.Request)) *fakeAPI {
	return newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		reply(w, r)
	})
}

// Default-class operations give up after the default timeout; clone, resize,
// migrate and wait-ip get the long one.
func TestTimeouts(t *testing.T) {
	var lastWait atomic.Value
	f := slowAPI(t, 300*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/volumes/clone"):
			jsonReply(w, 201, api.PathResult{Path: "/pool/web.qcow2"})
		case strings.HasSuffix(r.URL.Path, "/wait-ip"):
			var body api.WaitIPRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.TimeoutSeconds != nil {
				lastWait.Store(*body.TimeoutSeconds)
			}
			jsonReply(w, 200, api.WaitIPResult{Ip: "192.0.2.7"})
		case strings.HasSuffix(r.URL.Path, "/migrate-disk"):
			jsonReply(w, 200, api.VMInfo{Name: "web"})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	m := f.manager(t, WithTimeouts(100*time.Millisecond, 5*time.Second))

	start := time.Now()
	_, err := m.List()
	if err == nil || !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Fatalf("List: %v, want a timeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("List took %s", d)
	}
	var ae *app.Error
	if !app.AsError(err, &ae) || ae.Kind != app.KindBadGateway {
		t.Errorf("timeout kind: %v", err)
	}

	if p, err := m.CloneImage("/pool/base.qcow2", "web", "default", 0); err != nil || p != "/pool/web.qcow2" {
		t.Errorf("CloneImage = %q, %v (long timeout)", p, err)
	}
	if err := m.ResizeVolume("/pool/web.qcow2", 1<<30); err != nil {
		t.Errorf("ResizeVolume: %v (long timeout)", err)
	}
	if err := m.ResizeDisk("web", 20); err != nil {
		t.Errorf("ResizeDisk: %v (long timeout)", err)
	}
	if err := m.MigrateDisk("web", "fast"); err != nil {
		t.Errorf("MigrateDisk: %v (long timeout)", err)
	}
	if ip, err := m.WaitForIP("web", 90*time.Second); err != nil || ip != "192.0.2.7" {
		t.Errorf("WaitForIP = %q, %v", ip, err)
	}
	if got, _ := lastWait.Load().(int); got != 90 {
		t.Errorf("timeout_seconds sent = %d, want 90", got)
	}
	// Out of the daemon's range: clamped, not refused.
	if _, err := m.WaitForIP("web", 3*time.Hour); err != nil {
		t.Errorf("WaitForIP 3h: %v", err)
	}
	if got, _ := lastWait.Load().(int); got != 3600 {
		t.Errorf("timeout_seconds sent = %d, want 3600", got)
	}
}

func TestDefaultTimeouts(t *testing.T) {
	m, err := New("veritech01", "https://veritech01.labtwo:9473", testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.timeout != 30*time.Second || m.long != 30*time.Minute {
		t.Errorf("timeouts = %s / %s, want 30s / 30m", m.timeout, m.long)
	}
	if got := m.waitTimeout(2 * time.Minute); got != 30*time.Minute {
		t.Errorf("waitTimeout(2m) = %s", got)
	}
	if got := m.waitTimeout(time.Hour); got <= time.Hour {
		t.Errorf("waitTimeout(1h) = %s, want more than the daemon's own wait", got)
	}
}

// The client trusts only the system store unless a test injects a root: the
// httptest certificate is refused, and the token is not in the error.
func TestVerifiesServerCert(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached an unverified server")
	})
	m, err := New("veritech01", f.srv.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_, err = m.List()
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("List against an untrusted cert: %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Error("error contains the token")
	}
}

// A redirect is never followed: the token is for this daemon only.
func TestNoRedirect(t *testing.T) {
	var followed atomic.Bool
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	m := f.manager(t)
	if _, err := m.List(); err == nil {
		t.Fatal("redirect treated as success")
	}
	if followed.Load() {
		t.Fatal("redirect followed")
	}
}

// Define never sends guest credentials.
func TestDefineSendsNoCredentials(t *testing.T) {
	var got atomic.Value
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Store(string(b))
		w.WriteHeader(http.StatusNoContent)
	})
	m := f.manager(t)
	cfg := vmtool.DefaultConfig("web", "/pool/web.qcow2")
	cfg.SSHUser, cfg.SSHPass = "guest-user-x", "guest-pass-y"
	cfg.DiskCache = "unsafe"
	if err := m.Define(cfg); err != nil {
		t.Fatal(err)
	}
	body, _ := got.Load().(string)
	if !strings.Contains(body, `"disk_cache":"unsafe"`) {
		t.Errorf("define body %s lacks disk_cache", body)
	}
	if !strings.Contains(body, `"disk_path":"/pool/web.qcow2"`) {
		t.Errorf("define body %s", body)
	}
	if !strings.Contains(body, `"firmware":"efi"`) {
		t.Errorf("define body %s lacks the explicit default firmware", body)
	}
	for _, s := range []string{"guest-user-x", "guest-pass-y", "ssh"} {
		if strings.Contains(body, s) {
			t.Errorf("define body %s contains %q", body, s)
		}
	}
}

// getImageManifest is a read: one timeout is retried once. A stall that
// clears answers; two stalls fail; an error status is not retried.
func TestImageManifestRetriesOneTimeout(t *testing.T) {
	var calls atomic.Int32
	f := newFakeAPI(t, "1", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			select { // stall until the client gives up
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		sum := strings.Repeat("a", 64)
		jsonReply(w, 200, api.ImageManifestEntry{Name: "u.qcow2", Sha256: &sum, Size: ptr(int64(7))})
	})
	m := f.manager(t, WithTimeouts(150*time.Millisecond, 0))
	got, err := m.ImageManifest("u.qcow2")
	if err != nil {
		t.Fatalf("ImageManifest after one stall: %v", err)
	}
	if got.Size != 7 || calls.Load() != 2 {
		t.Errorf("got %+v after %d calls, want size 7 after 2", got, calls.Load())
	}
}

func TestImageManifestRetriesOnlyOnce(t *testing.T) {
	var calls atomic.Int32
	f := newFakeAPI(t, "1", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	m := f.manager(t, WithTimeouts(100*time.Millisecond, 0))
	if _, err := m.ImageManifest("u.qcow2"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (one try, one retry)", calls.Load())
	}
}

func TestImageManifestDoesNotRetryAnError(t *testing.T) {
	var calls atomic.Int32
	f := newFakeAPI(t, "1", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		jsonReply(w, 500, api.Error{Error: "boom"})
	})
	m := f.manager(t)
	if _, err := m.ImageManifest("u.qcow2"); err == nil {
		t.Fatal("want an error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

// Nothing else gains a retry: a stalled list is tried once.
func TestOtherReadsAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	f := newFakeAPI(t, "1", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	m := f.manager(t, WithTimeouts(100*time.Millisecond, 0))
	if _, err := m.List(); err == nil {
		t.Fatal("want a timeout")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}
