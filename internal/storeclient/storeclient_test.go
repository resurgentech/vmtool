package storeclient

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

	"github.com/resurgentech/vmtool/internal/storeapi"
)

// Placeholders only.
const (
	tok  = "PLACEHOLDER-not-a-secret-1"
	pass = "PLACEHOLDER-not-a-secret-2"
	uuid = "00000000-0000-4000-8000-000000000001"
)

// fakeStore is an in-process store on a loopback TLS listener. It records
// each request's method, URL (with query) and Authorization header.
type fakeStore struct {
	ts *httptest.Server
	mu sync.Mutex
	// reqs are "METHOD /path?query"; auths the Authorization headers.
	reqs  []string
	auths []string
	// h answers a request after recording it.
	h http.HandlerFunc
}

func newFake(t *testing.T, h http.HandlerFunc) *fakeStore {
	t.Helper()
	f := &fakeStore{h: h}
	f.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.mu.Unlock()
		f.h(w, r)
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *fakeStore) client(t *testing.T, token string) *Client {
	t.Helper()
	pool := f.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c, err := New(f.ts.URL, token, WithRootCAs(pool))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func js(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func TestWhoamiAndVersion(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			if r.Header.Get("Authorization") != "Bearer "+tok {
				js(w, 401, map[string]string{"error": "unauthorized"})
				return
			}
			js(w, 200, map[string]string{"name": "jared"})
		case "/version":
			js(w, 200, map[string]string{"version": "1.0.0"})
		default:
			http.NotFound(w, r)
		}
	})
	ctx := context.Background()
	name, err := f.client(t, tok).Whoami(ctx)
	if err != nil || name != "jared" {
		t.Fatalf("whoami %q %v", name, err)
	}
	v, err := f.client(t, tok).Version(ctx)
	if err != nil || v != "1.0.0" {
		t.Fatalf("version %q %v", v, err)
	}
	if _, err := f.client(t, "PLACEHOLDER-wrong").Whoami(ctx); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong token: %v", err)
	}
}

// Only GET /version itself goes without the bearer: an image or machine whose
// name is "version" is an authenticated route like any other.
func TestBearerOnEveryRouteButVersion(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			js(w, 200, map[string]string{"version": "1.0.0"})
		default:
			js(w, 404, map[string]string{"error": "not found"})
		}
	})
	c := f.client(t, tok)
	ctx := context.Background()
	_, _ = c.Version(ctx)
	_, _ = c.GetImage(ctx, "version")
	_, _ = c.GetMachine(ctx, "version")
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, req := range f.reqs {
		want := "Bearer " + tok
		if req == "GET /version" {
			want = ""
		}
		if f.auths[i] != want {
			t.Errorf("%s: Authorization %q, want %q", req, f.auths[i], want)
		}
	}
}

// The token rides only in the Authorization header: never in a URL, query or
// body, and never in an error, whatever the store answers.
func TestTokenOnlyInAuthorizationHeader(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		// A hostile or buggy store that echoes the token in an error.
		js(w, 500, map[string]string{"error": "boom " + tok})
	})
	c := f.client(t, tok)
	ctx := context.Background()
	in := storeapi.VMCredentialInput{Uuid: uuid, Username: "bot"}
	var errs []error
	_, e := c.Whoami(ctx)
	errs = append(errs, e)
	_, e = c.GetVMCredential(ctx, "hv1", "web", uuid)
	errs = append(errs, e)
	_, e = c.PutVMCredential(ctx, "hv1", "web", in)
	errs = append(errs, e)
	errs = append(errs, c.DeleteVMCredential(ctx, "hv1", "web"))
	_, e = c.ListMachines(ctx)
	errs = append(errs, e)
	_, e = c.ListImages(ctx)
	errs = append(errs, e)
	_, e = c.CreateImageTicket(ctx, "base")
	errs = append(errs, e)
	_, e = c.PushImage(ctx, "base", strings.Repeat("a", 64), 3, strings.NewReader("abc"))
	errs = append(errs, e)
	for i, err := range errs {
		if err == nil {
			t.Errorf("case %d: expected an error", i)
		} else if strings.Contains(err.Error(), tok) {
			t.Errorf("case %d: error carries the token: %v", i, err)
		}
	}
	for i, r := range f.reqs {
		if strings.Contains(r, tok) {
			t.Errorf("request %q carries the token", r)
		}
		if f.auths[i] != "Bearer "+tok {
			t.Errorf("request %q: Authorization %q", r, f.auths[i])
		}
	}
	for _, b := range bodies {
		if strings.Contains(b, tok) {
			t.Errorf("a body carries the token")
		}
	}
}

// F1: a store's error text is never quoted, so a store that echoes the
// request's token (bare or as a Bearer header, JSON or plain text) leaves it
// out of the error at every status.
func TestStoreErrorsQuoteNoBody(t *testing.T) {
	echo := "Authorization: Bearer " + tok + " bearer " + tok + " " + tok
	for _, status := range []int{400, 401, 403, 404, 409, 500, 502} {
		for _, plain := range []bool{false, true} {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				if plain {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, echo)
					return
				}
				js(w, status, map[string]string{"error": echo})
			})
			c := f.client(t, tok)
			ctx := context.Background()
			_, e1 := c.Whoami(ctx)
			_, e2 := c.GetVMCredential(ctx, "hv1", "web", uuid)
			_, e3 := c.ListImages(ctx)
			_, e4 := c.Version(ctx)
			for i, err := range []error{e1, e2, e3, e4} {
				if err == nil {
					t.Errorf("status %d case %d: no error", status, i)
					continue
				}
				if s := err.Error(); strings.Contains(s, tok) || strings.Contains(strings.ToLower(s), "bearer") {
					t.Errorf("status %d plain=%v case %d: error quotes the body: %v", status, plain, i, err)
				}
			}
		}
	}
}

func TestVMCredentialRoundTrip(t *testing.T) {
	var got storeapi.VMCredentialInput
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/credentials/vms/hv1/web" {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&got)
			js(w, 200, map[string]any{"host": "hv1", "name": "web", "uuid": got.Uuid, "updated": time.Now()})
		case http.MethodGet:
			switch r.URL.Query().Get("uuid") {
			case uuid:
				js(w, 200, map[string]any{"host": "hv1", "name": "web", "uuid": uuid, "username": got.Username,
					"password": pass, "private_key": "PLACEHOLDER-KEY-not-a-key", "public_key": "pub", "updated": time.Now()})
			case "00000000-0000-4000-8000-000000000002":
				js(w, 404, map[string]any{"error": "stale", "stale": true})
			default:
				js(w, 404, map[string]any{"error": "not found"})
			}
		case http.MethodDelete:
			w.WriteHeader(204)
		}
	})
	c := f.client(t, tok)
	ctx := context.Background()
	pw := pass
	if _, err := c.PutVMCredential(ctx, "hv1", "web", storeapi.VMCredentialInput{Uuid: uuid, Username: "bot", Password: &pw}); err != nil {
		t.Fatal(err)
	}
	if got.Username != "bot" || got.Password == nil || *got.Password != pass {
		t.Fatalf("server got %+v", got)
	}
	cred, err := c.GetVMCredential(ctx, "hv1", "web", uuid)
	if err != nil || cred.Password != pass || cred.PrivateKey == "" {
		t.Fatalf("get %+v %v", cred, err)
	}
	if _, err := c.GetVMCredential(ctx, "hv1", "web", "00000000-0000-4000-8000-000000000002"); !errors.Is(err, ErrStale) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale: %v", err)
	}
	if _, err := c.GetVMCredential(ctx, "hv1", "web", "00000000-0000-4000-8000-000000000003"); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrStale) {
		t.Fatalf("missing: %v", err)
	}
	if err := c.DeleteVMCredential(ctx, "hv1", "web"); err != nil {
		t.Fatal(err)
	}
}

func TestMachineRoundTrip(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/credentials/machines" && r.Method == http.MethodGet:
			js(w, 200, map[string]any{"machines": []map[string]string{{"name": "nuc1", "kind": "metal"}}})
		case r.URL.Path == "/v1/credentials/machines/nuc1" && r.Method == http.MethodPut:
			js(w, 200, map[string]any{"name": "nuc1", "kind": "metal", "updated": time.Now()})
		case r.URL.Path == "/v1/credentials/machines/nuc1" && r.Method == http.MethodGet:
			js(w, 200, map[string]any{"name": "nuc1", "kind": "metal", "address": "10.0.0.5", "username": "bot",
				"password": pass, "private_key": "", "public_key": "", "updated": time.Now()})
		case r.URL.Path == "/v1/credentials/machines/nuc1" && r.Method == http.MethodDelete:
			w.WriteHeader(204)
		default:
			js(w, 404, map[string]string{"error": "not found"})
		}
	})
	c := f.client(t, tok)
	ctx := context.Background()
	ms, err := c.ListMachines(ctx)
	if err != nil || len(ms) != 1 || ms[0].Name != "nuc1" {
		t.Fatalf("list %v %v", ms, err)
	}
	if _, err := c.PutMachine(ctx, "nuc1", storeapi.MachineCredentialInput{Kind: "metal", Address: "10.0.0.5", Username: "bot"}); err != nil {
		t.Fatal(err)
	}
	m, err := c.GetMachine(ctx, "nuc1")
	if err != nil || m.Address != "10.0.0.5" {
		t.Fatalf("get %+v %v", m, err)
	}
	if err := c.DeleteMachine(ctx, "nuc1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetMachine(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestImages(t *testing.T) {
	blob := []byte("PLACEHOLDER image bytes")
	sum := sha256.Sum256(blob)
	sha := hex.EncodeToString(sum[:])
	var pushed []byte
	var pushedLen int64
	var pushedSHA string
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		img := map[string]any{"name": "base", "sha256": sha, "size": len(blob), "updated": time.Now()}
		switch {
		case r.URL.Path == "/v1/images" && r.Method == http.MethodGet:
			js(w, 200, map[string]any{"images": []any{img}})
		case r.URL.Path == "/v1/images/base" && r.Method == http.MethodGet:
			js(w, 200, img)
		case r.URL.Path == "/v1/images/base" && r.Method == http.MethodPut:
			pushed, _ = io.ReadAll(r.Body)
			pushedLen, pushedSHA = r.ContentLength, r.Header.Get("X-Image-SHA256")
			js(w, 201, img)
		case r.URL.Path == "/v1/images/base" && r.Method == http.MethodDelete:
			w.WriteHeader(204)
		case r.URL.Path == "/v1/images/base/blob":
			w.Header().Set("X-Image-SHA256", sha)
			w.Header().Set("Content-Length", "23")
			_, _ = w.Write(blob)
		case r.URL.Path == "/v1/images/base/tickets" && r.Method == http.MethodPost:
			js(w, 201, map[string]any{"ticket": "PLACEHOLDER-ticket", "expires_at": time.Now().Add(15 * time.Minute),
				"image": "base", "sha256": sha, "size": len(blob)})
		default:
			js(w, 404, map[string]string{"error": "not found"})
		}
	})
	c := f.client(t, tok)
	ctx := context.Background()
	l, err := c.ListImages(ctx)
	if err != nil || len(l) != 1 || l[0].Sha256 != sha {
		t.Fatalf("list %v %v", l, err)
	}
	if im, err := c.GetImage(ctx, "base"); err != nil || im.Size != int64(len(blob)) {
		t.Fatalf("get %v %v", im, err)
	}
	if _, err := c.GetImage(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := c.PushImage(ctx, "base", sha, int64(len(blob)), strings.NewReader(string(blob))); err != nil {
		t.Fatal(err)
	}
	if string(pushed) != string(blob) || pushedLen != int64(len(blob)) || pushedSHA != sha {
		t.Fatalf("push: body %q len %d sha %q", pushed, pushedLen, pushedSHA)
	}
	rc, gotSHA, size, err := c.OpenImage(ctx, "base")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != string(blob) || gotSHA != sha || size != int64(len(blob)) {
		t.Fatalf("blob %q %q %d", b, gotSHA, size)
	}
	if _, _, _, err := c.OpenImage(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("blob missing: %v", err)
	}
	tk, err := c.CreateImageTicket(ctx, "base")
	if err != nil || tk.Ticket != "PLACEHOLDER-ticket" {
		t.Fatalf("ticket %+v %v", tk, err)
	}
	if err := c.DeleteImage(ctx, "base"); err != nil {
		t.Fatal(err)
	}
}

func TestNewRefusesBadURLAndEchoesNothing(t *testing.T) {
	for _, u := range []string{"http://PLACEHOLDER-HOST", "", "https://", "://x"} {
		if _, err := New(u, tok); err == nil {
			t.Errorf("accepted %q", u)
		} else if strings.Contains(err.Error(), tok) {
			t.Errorf("error carries the token: %v", err)
		}
	}
}

// A cancelled or expired context ends the call.
func TestContextDeadline(t *testing.T) {
	release := make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.client(t, tok).Whoami(ctx)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	if strings.Contains(err.Error(), tok) {
		t.Fatalf("error carries the token: %v", err)
	}
}

func TestInventoryClient(t *testing.T) {
	var put storeapi.HostReport
	yes := true
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "PUT /v1/hosts/hv1/inventory":
			_ = json.NewDecoder(r.Body).Decode(&put)
			js(w, 200, map[string]any{"listed": 1, "newly_gone": 2, "relisted": 3, "stale": false})
		case "PUT /v1/hosts/hv2/inventory":
			js(w, 403, map[string]any{"error": "forbidden: " + tok})
		case "GET /v1/inventory":
			js(w, 200, map[string]any{"hosts": []any{}, "vms": []any{}, "machines": []map[string]string{{"name": "m1", "kind": "metal"}}})
		case "POST /v1/inventory/hv1/vm1/delist":
			js(w, 200, map[string]any{"uuid": uuid, "credentials_kept_until": time.Now(), "had_credentials": true})
		case "POST /v1/inventory/hv1/nope/delist", "DELETE /v1/inventory/hv1/nope":
			js(w, 404, map[string]any{"error": "not found"})
		case "DELETE /v1/inventory/hv1/vm1":
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	})
	c := f.client(t, tok)
	ctx := context.Background()

	in := storeapi.HostReport{GeneratedAt: time.Now(), DaemonVersion: "2.6.0", Vms: []storeapi.ReportVM{{Name: "vm1", Uuid: uuid, DisksUnreadable: &yes}}}
	r, err := c.PutHostInventory(ctx, "hv1", in)
	if err != nil || r.Listed != 1 || r.NewlyGone != 2 || r.Relisted != 3 {
		t.Fatalf("put = %+v %v", r, err)
	}
	if len(put.Vms) != 1 || put.Vms[0].Uuid != uuid || put.DaemonVersion != "2.6.0" || put.Vms[0].DisksUnreadable == nil || !*put.Vms[0].DisksUnreadable {
		t.Fatalf("server got %+v", put)
	}
	if _, err := c.PutHostInventory(ctx, "hv2", in); !errors.Is(err, ErrForbidden) {
		t.Fatalf("403 = %v, want ErrForbidden", err)
	} else if strings.Contains(err.Error(), tok) {
		t.Fatalf("error carries the token or a body: %v", err)
	}

	inv, err := c.Inventory(ctx, "", false)
	if err != nil || len(inv.Machines) != 1 {
		t.Fatalf("inventory = %+v %v", inv, err)
	}
	if _, err := c.Inventory(ctx, "hv1", true); err != nil {
		t.Fatal(err)
	}
	d, err := c.Delist(ctx, "hv1", "vm1")
	if err != nil || d.Uuid != uuid || !d.HadCredentials {
		t.Fatalf("delist = %+v %v", d, err)
	}
	if _, err := c.Delist(ctx, "hv1", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delist 404 = %v", err)
	}
	if err := c.DeleteInventoryRow(ctx, "hv1", "vm1", uuid); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteInventoryRow(ctx, "hv1", "nope", uuid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete 404 = %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	want := []string{
		"PUT /v1/hosts/hv1/inventory", "PUT /v1/hosts/hv2/inventory",
		"GET /v1/inventory", "GET /v1/inventory?gone=true&host=hv1",
		"POST /v1/inventory/hv1/vm1/delist", "POST /v1/inventory/hv1/nope/delist",
		"DELETE /v1/inventory/hv1/vm1?uuid=" + uuid, "DELETE /v1/inventory/hv1/nope?uuid=" + uuid,
	}
	if strings.Join(f.reqs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(f.reqs, "\n"), strings.Join(want, "\n"))
	}
	for i, a := range f.auths {
		if a != "Bearer "+tok {
			t.Errorf("request %d Authorization = %q", i, a)
		}
	}
	for _, q := range f.reqs {
		if strings.Contains(q, tok) {
			t.Errorf("a URL carries the token: %s", q)
		}
	}
}

func TestPutVMNotes(t *testing.T) {
	var got storeapi.VMNotesInput
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "PUT /v1/inventory/hv1/vm1/notes":
			_ = json.NewDecoder(r.Body).Decode(&got)
			js(w, 200, map[string]any{"host": "hv1", "name": "vm1", "uuid": uuid, "comment": "placeholder comment",
				"origin": map[string]string{"image": "img1", "clone": "thin"}})
		case "PUT /v1/inventory/hv1/gone/notes":
			js(w, 409, map[string]any{"error": "the VM is gone from the inventory " + tok})
		case "PUT /v1/inventory/hv1/nope/notes":
			js(w, 404, map[string]any{"error": "not found"})
		default:
			http.NotFound(w, r)
		}
	})
	c := f.client(t, tok)
	ctx := context.Background()
	u, comment, img, clone := uuid, "placeholder comment", "img1", "thin"
	n, err := c.PutVMNotes(ctx, "hv1", "vm1", VMNotesInput{Uuid: &u, Comment: &comment, Origin: &VMOrigin{Image: &img, Clone: &clone}})
	if err != nil || n.Uuid != uuid || n.Comment != comment || n.Origin == nil || *n.Origin.Image != "img1" {
		t.Fatalf("notes = %+v %v", n, err)
	}
	if got.Uuid == nil || *got.Uuid != uuid || got.Comment == nil || got.Origin == nil || *got.Origin.Clone != "thin" {
		t.Fatalf("server got %+v", got)
	}
	_, err = c.PutVMNotes(ctx, "hv1", "gone", VMNotesInput{Comment: &comment})
	if !errors.Is(err, ErrNotesRefused) || strings.Contains(err.Error(), tok) {
		t.Fatalf("409 = %v", err)
	}
	if _, err := c.PutVMNotes(ctx, "hv1", "nope", VMNotesInput{Comment: &comment}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 = %v", err)
	}
}
