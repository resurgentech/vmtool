package storeclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/store"
	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/store/db"
	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/internal/tokenhash"
)

// Placeholder host token.
const hostTok = "PLACEHOLDER-not-a-secret-3"

// prefixedStore is the real store handler (in-process, httptest TLS, no
// network) serving under /vmstore. It records each request's escaped path
// and Authorization header before the store sees it.
type prefixedStore struct {
	ts    *httptest.Server
	mu    sync.Mutex
	paths []string
	auths []string
}

func newPrefixedStore(t *testing.T) *prefixedStore {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(dir, "store.db"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	c := &store.Config{
		Listen:     "127.0.0.1:8095",
		PathPrefix: "/vmstore",
		Tokens:     map[string]string{"alice": tokenhash.Hash(tok)},
		HostTokens: map[string]string{"hv1": tokenhash.Hash(hostTok)},
	}
	h := store.NewServer(c, slog.New(slog.NewJSONHandler(io.Discard, nil)), store.Deps{Blob: blob.NewMemory(), DB: d}).Handler()
	p := &prefixedStore{}
	p.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.paths = append(p.paths, r.Method+" "+r.URL.EscapedPath())
		p.auths = append(p.auths, r.Header.Get("Authorization"))
		p.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(p.ts.Close)
	return p
}

func (p *prefixedStore) client(t *testing.T, base, token string) *Client {
	t.Helper()
	pool := p.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c, err := New(base, token, WithRootCAs(pool))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// A base URL with a path, with or without the trailing slash, puts every call
// under it; the version probe alone goes without the bearer.
func TestClientUnderPathPrefix(t *testing.T) {
	for _, suffix := range []string{"/vmstore", "/vmstore/"} {
		t.Run(suffix, func(t *testing.T) {
			p := newPrefixedStore(t)
			c := p.client(t, p.ts.URL+suffix, tok)
			ctx := context.Background()

			if v, err := c.Version(ctx); err != nil || v != "1.4.0" {
				t.Fatalf("version %q %v", v, err)
			}
			if name, err := c.Whoami(ctx); err != nil || name != "alice" {
				t.Fatalf("whoami %q %v", name, err)
			}
			img := []byte("PLACEHOLDER-image-bytes")
			h := sha256.Sum256(img)
			sum := hex.EncodeToString(h[:])
			if _, err := c.PushImage(ctx, "base.qcow2", sum, int64(len(img)), bytes.NewReader(img)); err != nil {
				t.Fatalf("push: %v", err)
			}
			if imgs, err := c.ListImages(ctx); err != nil || len(imgs) != 1 {
				t.Fatalf("list = %v %v", imgs, err)
			}
			body, gotSum, _, err := c.OpenImage(ctx, "base.qcow2")
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			got, _ := io.ReadAll(body)
			body.Close()
			if !bytes.Equal(got, img) || gotSum != sum {
				t.Fatalf("blob %q sum %s", got, gotSum)
			}
			if _, err := c.CreateImageTicket(ctx, "base.qcow2"); err != nil {
				t.Fatalf("ticket: %v", err)
			}
			if _, err := c.Networks(ctx); err != nil {
				t.Fatalf("networks: %v", err)
			}

			p.mu.Lock()
			defer p.mu.Unlock()
			if len(p.paths) < 7 {
				t.Fatalf("only %d requests: %v", len(p.paths), p.paths)
			}
			for i, req := range p.paths {
				_, path, _ := strings.Cut(req, " ")
				if !strings.HasPrefix(path, "/vmstore/") {
					t.Errorf("%s is not under /vmstore/", req)
				}
				want := "Bearer " + tok
				if req == "GET /vmstore/version" {
					want = ""
				}
				if p.auths[i] != want {
					t.Errorf("%s: Authorization %q, want %q", req, p.auths[i], want)
				}
			}
			for _, must := range []string{"GET /vmstore/version", "GET /vmstore/v1/whoami"} {
				var seen bool
				for _, req := range p.paths {
					seen = seen || req == must
				}
				if !seen {
					t.Errorf("no %s in %v", must, p.paths)
				}
			}
		})
	}
}

// A hypervisor's two host-token calls, the inventory report and the
// network-registry read, go through under the prefix.
func TestHostCallsUnderPathPrefix(t *testing.T) {
	p := newPrefixedStore(t)
	c := p.client(t, p.ts.URL+"/vmstore", hostTok)
	ctx := context.Background()
	in := storeapi.HostReport{GeneratedAt: time.Now(), DaemonVersion: "2.8.0", Vms: []storeapi.ReportVM{}}
	r, err := c.PutHostInventory(ctx, "hv1", in)
	if err != nil || r == nil || r.Stale {
		t.Fatalf("report = %+v %v", r, err)
	}
	hn, err := c.HostNetworks(ctx, "hv1")
	if err != nil || hn.Host != "hv1" {
		t.Fatalf("host networks = %+v %v", hn, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	want := []string{"PUT /vmstore/v1/hosts/hv1/inventory", "GET /vmstore/v1/hosts/hv1/networks"}
	if strings.Join(p.paths, ",") != strings.Join(want, ",") {
		t.Errorf("requests %v, want %v", p.paths, want)
	}
	for i, a := range p.auths {
		if a != "Bearer "+hostTok {
			t.Errorf("%s: Authorization %q", p.paths[i], a)
		}
	}
}
