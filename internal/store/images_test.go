package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/store/blob"
	"github.com/jaredeh/vmtool/internal/store/db"
	"github.com/jaredeh/vmtool/internal/storeapi"
	"github.com/jaredeh/vmtool/internal/tokenhash"
)

func openTestDB(t *testing.T) *db.DB {
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
	return d
}

// trackBlob wraps the in-process double and remembers which keys exist, so a
// test can see that a failed push left no object behind.
type trackBlob struct {
	*blob.Memory
	mu   sync.Mutex
	keys map[string]bool
}

func newTrackBlob() *trackBlob { return &trackBlob{Memory: blob.NewMemory(), keys: map[string]bool{}} }

func (b *trackBlob) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := b.Memory.Put(ctx, key, r, size); err != nil {
		return err
	}
	b.mu.Lock()
	b.keys[key] = true
	b.mu.Unlock()
	return nil
}

func (b *trackBlob) Delete(ctx context.Context, key string) error {
	b.mu.Lock()
	delete(b.keys, key)
	b.mu.Unlock()
	return b.Memory.Delete(ctx, key)
}

func (b *trackBlob) Keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for k := range b.keys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lockedBuf is a log sink safe against the server goroutine still writing the
// request's log line while the test reads.
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

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	srv   *Server
	ts    *httptest.Server
	blob  *trackBlob
	db    *db.DB
	logs  *lockedBuf
	clock *clock
	// seen collects every response (status line, headers, body) except the
	// ticket mint body, for the secrecy test.
	mu   sync.Mutex
	seen bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{blob: newTrackBlob(), db: openTestDB(t), logs: &lockedBuf{}, clock: &clock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}}
	c := &Config{
		Listen: "127.0.0.1:8095",
		Tokens: map[string]string{"alice": tokenhash.Hash(tokA), "bob": tokenhash.Hash(tokB)},
	}
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.srv = NewServer(c, log, Deps{Blob: e.blob, DB: e.db, Now: e.clock.now})
	e.ts = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

// call sends a request and records the response in e.seen (unless it is the
// ticket mint, whose body is allowed to carry the ticket).
func (e *env) call(t *testing.T, method, p, auth string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.ts.URL+p, body)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return e.send(t, req)
}

func (e *env) send(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !(req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/tickets") && resp.StatusCode == 201) {
		e.mu.Lock()
		resp.Header.Write(&e.seen)
		if !strings.Contains(resp.Header.Get("Content-Type"), "octet-stream") {
			e.seen.Write(b)
		}
		e.mu.Unlock()
	}
	return resp, b
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func payload(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func (e *env) push(t *testing.T, name string, body []byte, digest string) (*http.Response, []byte) {
	t.Helper()
	return e.call(t, "PUT", "/v1/images/"+name, "Bearer "+tokA, bytes.NewReader(body), map[string]string{"X-Image-SHA256": digest})
}

func (e *env) mustPush(t *testing.T, name string, body []byte) {
	t.Helper()
	resp, b := e.push(t, name, body, sum(body))
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		t.Fatalf("push %s = %d %s", name, resp.StatusCode, b)
	}
}

func (e *env) mint(t *testing.T, name string) storeapi.Ticket {
	t.Helper()
	resp, b := e.call(t, "POST", "/v1/images/"+name+"/tickets", "Bearer "+tokA, nil, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("mint = %d %s", resp.StatusCode, b)
	}
	var tk storeapi.Ticket
	if err := json.Unmarshal(b, &tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func (e *env) blobWith(t *testing.T, name, auth string) (*http.Response, []byte) {
	t.Helper()
	return e.call(t, "GET", "/v1/images/"+name+"/blob", auth, nil, nil)
}

func TestPushAndServeMultiMiB(t *testing.T) {
	e := newEnv(t)
	body := payload(1, 5<<20+37)
	d := sum(body)
	resp, b := e.push(t, "base-1.img", body, d)
	if resp.StatusCode != 201 {
		t.Fatalf("push = %d %s", resp.StatusCode, b)
	}
	var img storeapi.Image
	if err := json.Unmarshal(b, &img); err != nil || img.Name != "base-1.img" || img.Sha256 != d || img.Size != int64(len(body)) {
		t.Fatalf("push body %s (%v)", b, err)
	}
	resp, b = e.call(t, "GET", "/v1/images", "Bearer "+tokA, nil, nil)
	var list storeapi.ImageList
	if resp.StatusCode != 200 || json.Unmarshal(b, &list) != nil || len(list.Images) != 1 || list.Images[0].Sha256 != d {
		t.Fatalf("list = %d %s", resp.StatusCode, b)
	}
	resp, b = e.call(t, "GET", "/v1/images/base-1.img", "Bearer "+tokA, nil, nil)
	if resp.StatusCode != 200 || json.Unmarshal(b, &img) != nil || img.Size != int64(len(body)) || img.Updated.IsZero() {
		t.Fatalf("metadata = %d %s", resp.StatusCode, b)
	}
	resp, b = e.blobWith(t, "base-1.img", "Bearer "+tokB)
	if resp.StatusCode != 200 || !bytes.Equal(b, body) {
		t.Fatalf("blob = %d, %d bytes", resp.StatusCode, len(b))
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q", got)
	}
	if got := resp.Header.Get("X-Image-SHA256"); got != d {
		t.Errorf("X-Image-SHA256 = %q", got)
	}
	// the same digest again is acknowledged without a rewrite
	resp, _ = e.push(t, "base-1.img", body, d)
	if resp.StatusCode != 200 {
		t.Errorf("repush of same digest = %d, want 200", resp.StatusCode)
	}
	if k := e.blob.Keys(); len(k) != 1 {
		t.Errorf("objects = %v", k)
	}
}

func TestPushRejectsWrongHash(t *testing.T) {
	e := newEnv(t)
	body := payload(2, 1<<20)
	resp, b := e.push(t, "x", body, sum([]byte("other")))
	if resp.StatusCode != 400 {
		t.Fatalf("wrong hash = %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.call(t, "GET", "/v1/images/x", "Bearer "+tokA, nil, nil); resp.StatusCode != 404 {
		t.Errorf("indexed after a bad push: %d", resp.StatusCode)
	}
	if k := e.blob.Keys(); len(k) != 0 {
		t.Errorf("objects left: %v", k)
	}
}

// A body shorter than Content-Length (the client hung up) is a 400 and
// leaves nothing. Go's HTTP client refuses to send one, so the handler is
// called directly.
func TestPushRejectsShortBody(t *testing.T) {
	e := newEnv(t)
	body := payload(3, 1<<20)
	req := httptest.NewRequest("PUT", "/v1/images/x", bytes.NewReader(body[:1000]))
	req.ContentLength = int64(len(body))
	req.Header.Set("Authorization", "Bearer "+tokA)
	req.Header.Set("X-Image-SHA256", sum(body))
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("short body = %d %s", rec.Code, rec.Body)
	}
	if _, err := e.db.GetImage(context.Background(), "x"); err == nil {
		t.Error("indexed after a short push")
	}
	if k := e.blob.Keys(); len(k) != 0 {
		t.Errorf("objects left: %v", k)
	}
}

func TestReplaceAndFailedReplace(t *testing.T) {
	e := newEnv(t)
	v1, v2, v3 := payload(10, 300<<10), payload(11, 310<<10), payload(12, 320<<10)
	e.mustPush(t, "img", v1)
	k1 := e.blob.Keys()
	e.mustPush(t, "img", v2)
	resp, b := e.blobWith(t, "img", "Bearer "+tokA)
	if resp.StatusCode != 200 || !bytes.Equal(b, v2) {
		t.Fatalf("after replace blob = %d", resp.StatusCode)
	}
	var img storeapi.Image
	_, b = e.call(t, "GET", "/v1/images/img", "Bearer "+tokA, nil, nil)
	if json.Unmarshal(b, &img) != nil || img.Sha256 != sum(v2) {
		t.Fatalf("metadata %s", b)
	}
	k2 := e.blob.Keys()
	if len(k2) != 1 || k2[0] == k1[0] {
		t.Fatalf("v1 object not gone: before %v after %v", k1, k2)
	}
	// a failed v3 leaves v2 served and no stray object
	if resp, _ := e.push(t, "img", v3, sum([]byte("nope"))); resp.StatusCode != 400 {
		t.Fatalf("bad v3 = %d", resp.StatusCode)
	}
	resp, b = e.blobWith(t, "img", "Bearer "+tokA)
	if resp.StatusCode != 200 || !bytes.Equal(b, v2) {
		t.Fatalf("after failed v3 blob = %d", resp.StatusCode)
	}
	if k := e.blob.Keys(); len(k) != 1 || k[0] != k2[0] {
		t.Errorf("objects = %v, want %v", k, k2)
	}
	// a liar claiming the live digest with other bytes does not touch the live object
	if resp, _ := e.push(t, "img", v3, sum(v2)); resp.StatusCode != 200 {
		t.Logf("repush claiming live digest = %d", resp.StatusCode)
	}
	if _, b = e.blobWith(t, "img", "Bearer "+tokA); !bytes.Equal(b, v2) {
		t.Fatal("live bytes changed by a push claiming the live digest")
	}
}

func TestConcurrentPushIs409(t *testing.T) {
	e := newEnv(t)
	body := payload(20, 200<<10)
	pr, pw := io.Pipe()
	req, err := http.NewRequest("PUT", e.ts.URL+"/v1/images/img", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	req.Header.Set("Authorization", "Bearer "+tokA)
	req.Header.Set("X-Image-SHA256", sum(body))
	t.Cleanup(func() { pw.Close() })
	done := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- -1
			return
		}
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	// Once this write returns the handler is reading the body, so it holds the name.
	wrote := make(chan error, 1)
	go func() { _, err := pw.Write(body[:1000]); wrote <- err }()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		pw.Close()
		t.Fatal("the first push never read its body")
	}
	resp, b := e.push(t, "img", body, sum(body))
	if resp.StatusCode != 409 {
		t.Fatalf("second push = %d %s", resp.StatusCode, b)
	}
	// a different name is not blocked
	e.mustPush(t, "other", payload(21, 1000))
	// deleting the busy name is refused too
	if resp, _ := e.call(t, "DELETE", "/v1/images/img", "Bearer "+tokA, nil, nil); resp.StatusCode != 409 {
		t.Errorf("delete during push = %d", resp.StatusCode)
	}
	if _, err := pw.Write(body[1000:]); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	if st := <-done; st != 201 {
		t.Fatalf("first push = %d", st)
	}
	// the lock is released afterwards
	if resp, _ := e.push(t, "img", body, sum(body)); resp.StatusCode != 200 {
		t.Errorf("push after release = %d", resp.StatusCode)
	}
}

func TestPushHeaderValidation(t *testing.T) {
	e := newEnv(t)
	body := []byte("PLACEHOLDER-image-bytes")
	// chunked: no Content-Length
	req, _ := http.NewRequest("PUT", e.ts.URL+"/v1/images/x", io.NopCloser(bytes.NewReader(body)))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+tokA)
	req.Header.Set("X-Image-SHA256", sum(body))
	if resp, b := e.send(t, req); resp.StatusCode != 411 {
		t.Errorf("chunked = %d %s", resp.StatusCode, b)
	}
	for name, h := range map[string]string{
		"missing":   "",
		"short":     sum(body)[:63],
		"uppercase": strings.ToUpper(sum(body)),
		"non-hex":   strings.Repeat("g", 64),
	} {
		hdr := map[string]string{}
		if h != "" {
			hdr["X-Image-SHA256"] = h
		}
		resp, b := e.call(t, "PUT", "/v1/images/x", "Bearer "+tokA, bytes.NewReader(body), hdr)
		if resp.StatusCode != 400 {
			t.Errorf("%s digest = %d %s", name, resp.StatusCode, b)
		}
	}
	if k := e.blob.Keys(); len(k) != 0 {
		t.Errorf("objects left: %v", k)
	}
}

func TestImageNameValidation(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"-x", ".x", "a..b", "a%20b", strings.Repeat("a", 129), "a%2Fb", "a%00b"} {
		for _, m := range []string{"GET /v1/images/", "DELETE /v1/images/"} {
			method, base, _ := strings.Cut(m, " ")
			resp, b := e.call(t, method, base+n, "Bearer "+tokA, nil, nil)
			if resp.StatusCode != 400 {
				t.Errorf("%s %q = %d", method, n, resp.StatusCode)
			}
			if !strings.Contains(string(b), "name") || strings.Contains(string(b), n) {
				t.Errorf("%s %q body %q", method, n, b)
			}
		}
		if resp, _ := e.call(t, "POST", "/v1/images/"+n+"/tickets", "Bearer "+tokA, nil, nil); resp.StatusCode != 400 {
			t.Errorf("ticket for %q = %d", n, resp.StatusCode)
		}
		body := []byte("x")
		if resp, _ := e.push(t, n, body, sum(body)); resp.StatusCode != 400 {
			t.Errorf("push %q = %d", n, resp.StatusCode)
		}
	}
	if k := e.blob.Keys(); len(k) != 0 {
		t.Errorf("objects left: %v", k)
	}
}

func TestDelete(t *testing.T) {
	e := newEnv(t)
	e.mustPush(t, "img", payload(30, 5000))
	if resp, _ := e.call(t, "DELETE", "/v1/images/img", "Bearer "+tokA, nil, nil); resp.StatusCode != 204 {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	for _, p := range []string{"/v1/images/img", "/v1/images/img/blob"} {
		if resp, _ := e.call(t, "GET", p, "Bearer "+tokA, nil, nil); resp.StatusCode != 404 {
			t.Errorf("GET %s after delete = %d", p, resp.StatusCode)
		}
	}
	if resp, _ := e.call(t, "DELETE", "/v1/images/img", "Bearer "+tokA, nil, nil); resp.StatusCode != 404 {
		t.Errorf("second delete = %d", resp.StatusCode)
	}
	if resp, _ := e.call(t, "POST", "/v1/images/img/tickets", "Bearer "+tokA, nil, nil); resp.StatusCode != 404 {
		t.Errorf("ticket for a missing image = %d", resp.StatusCode)
	}
	if k := e.blob.Keys(); len(k) != 0 {
		t.Errorf("object left: %v", k)
	}
}

func TestTicketOneUse(t *testing.T) {
	e := newEnv(t)
	body := payload(40, 2<<20)
	e.mustPush(t, "A", body)
	tk := e.mint(t, "A")
	if tk.Image != "A" || tk.Sha256 != sum(body) || tk.Size != int64(len(body)) || len(tk.Ticket) < 43 {
		t.Fatalf("ticket = %+v", tk)
	}
	if want := e.clock.now().Add(15 * time.Minute); !tk.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %v, want %v", tk.ExpiresAt, want)
	}
	if tk2 := e.mint(t, "A"); tk2.Ticket == tk.Ticket {
		t.Error("two tickets are equal")
	}
	resp, b := e.blobWith(t, "A", "Ticket "+tk.Ticket)
	if resp.StatusCode != 200 || !bytes.Equal(b, body) || resp.Header.Get("X-Image-SHA256") != sum(body) {
		t.Fatalf("first use = %d", resp.StatusCode)
	}
	resp, b = e.blobWith(t, "A", "Ticket "+tk.Ticket)
	if resp.StatusCode != 401 || bytes.Contains(b, body[:64]) {
		t.Fatalf("second use = %d", resp.StatusCode)
	}
	// the log names the ticket's issuer, never the ticket
	if !strings.Contains(e.logs.String(), "ticket:alice") {
		t.Errorf("log lacks the ticket caller:\n%s", e.logs.String())
	}
}

func TestTicketMintNeedsBearer(t *testing.T) {
	e := newEnv(t)
	e.mustPush(t, "A", []byte("PLACEHOLDER-image-bytes"))
	tk := e.mint(t, "A")
	for _, auth := range []string{"", "Ticket " + tk.Ticket, "Bearer PLACEHOLDER-wrong"} {
		if resp, _ := e.call(t, "POST", "/v1/images/A/tickets", auth, nil, nil); resp.StatusCode != 401 {
			t.Errorf("mint with %q = %d", auth, resp.StatusCode)
		}
	}
}

func TestTicketOtherImageExpiredReplaced(t *testing.T) {
	e := newEnv(t)
	a, b := payload(50, 1000), payload(51, 1000)
	e.mustPush(t, "A", a)
	e.mustPush(t, "B", b)
	tk := e.mint(t, "A")

	// another image's blob: refused, and A's ticket is not spent by it
	resp, body := e.blobWith(t, "B", "Ticket "+tk.Ticket)
	if resp.StatusCode != 401 || bytes.Equal(body, b) {
		t.Fatalf("A's ticket on B = %d", resp.StatusCode)
	}

	// unclean and oddly encoded paths never reach another image
	for _, p := range []string{"/v1/images/A/../B/blob", "/v1/images/A%2F..%2FB/blob", "/v1/images/./B/blob", "/v1/images//B/blob", "/v1/images/B/blob/"} {
		resp, body := e.call(t, "GET", p, "Ticket "+tk.Ticket, nil, nil)
		if resp.StatusCode == 200 || bytes.Equal(body, b) {
			t.Errorf("%s with A's ticket = %d", p, resp.StatusCode)
		}
	}
	// an encoded dot in A's own name is not A's clean path either
	if resp, _ := e.call(t, "GET", "/v1/images/%41/blob", "Ticket "+tk.Ticket, nil, nil); resp.StatusCode != 401 {
		t.Errorf("percent-encoded name with a ticket = %d, want 401", resp.StatusCode)
	}

	// HEAD is not a download and spends nothing
	if resp, _ := e.call(t, "HEAD", "/v1/images/A/blob", "Ticket "+tk.Ticket, nil, nil); resp.StatusCode != 401 {
		t.Errorf("HEAD with a ticket = %d", resp.StatusCode)
	}

	// still good for A
	if resp, body := e.blobWith(t, "A", "Ticket "+tk.Ticket); resp.StatusCode != 200 || !bytes.Equal(body, a) {
		t.Fatalf("A's ticket after refusals = %d", resp.StatusCode)
	}

	// expired
	tk = e.mint(t, "A")
	e.clock.advance(15*time.Minute + time.Second)
	if resp, _ := e.blobWith(t, "A", "Ticket "+tk.Ticket); resp.StatusCode != 401 {
		t.Errorf("expired ticket = %d", resp.StatusCode)
	}

	// the image changed after issue: never serve other bytes
	tk = e.mint(t, "A")
	a2 := payload(52, 1500)
	e.mustPush(t, "A", a2)
	resp, body = e.blobWith(t, "A", "Ticket "+tk.Ticket)
	if resp.StatusCode != 401 || bytes.Equal(body, a2) {
		t.Errorf("ticket after replace = %d", resp.StatusCode)
	}
	// and after delete
	tk = e.mint(t, "A")
	e.call(t, "DELETE", "/v1/images/A", "Bearer "+tokA, nil, nil)
	if resp, _ := e.blobWith(t, "A", "Ticket "+tk.Ticket); resp.StatusCode != 401 && resp.StatusCode != 404 {
		t.Errorf("ticket after delete = %d", resp.StatusCode)
	}
}

func TestTicketRefusedOnEveryOtherRoute(t *testing.T) {
	e := newEnv(t)
	body := payload(60, 4000)
	e.mustPush(t, "A", body)
	tk := e.mint(t, "A")
	n := 0
	for op, public := range specOps(t) {
		if public || op == "GET /v1/images/{name}/blob" {
			continue
		}
		method, p, _ := strings.Cut(op, " ")
		p = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(p, "{name}", "A"), "{host}", "hv1"), "{mac}", "52:54:00:00:00:01")
		hdr := map[string]string{"X-Image-SHA256": sum(body)}
		resp, b := e.call(t, method, p, "Ticket "+tk.Ticket, bytes.NewReader(body), hdr)
		if resp.StatusCode != 401 {
			t.Errorf("%s with a ticket = %d %s", op, resp.StatusCode, b)
		}
		n++
	}
	if n != 30 {
		t.Errorf("checked %d routes, want 30", n)
	}
	// none of that spent the ticket, and A is untouched
	resp, got := e.blobWith(t, "A", "Ticket "+tk.Ticket)
	if resp.StatusCode != 200 || !bytes.Equal(got, body) {
		t.Fatalf("redeem after refusals = %d", resp.StatusCode)
	}
}

// Neither a token nor a ticket (nor its hash) reaches a log line, an error
// body or a header; the one place a ticket appears is the mint response.
func TestNothingSecretLeaks(t *testing.T) {
	e := newEnv(t)
	body := payload(70, 3000)
	e.mustPush(t, "A", body)
	tk := e.mint(t, "A")
	ticketHash := sum([]byte(tk.Ticket))

	e.blobWith(t, "A", "Ticket "+tk.Ticket) // use
	e.blobWith(t, "A", "Ticket "+tk.Ticket) // replay -> 401
	e.blobWith(t, "B", "Ticket "+tk.Ticket) // other image -> 401
	e.call(t, "GET", "/v1/whoami", "Ticket "+tk.Ticket, nil, nil)
	e.call(t, "GET", "/v1/images/A/../B/blob", "Ticket "+tk.Ticket, nil, nil)
	e.call(t, "GET", "/v1/images/A/blob", "Bearer "+tokA+"x", nil, nil)
	e.call(t, "GET", "/v1/images/missing/blob", "Bearer "+tokA, nil, nil)
	e.push(t, "A", body, sum([]byte("x")))
	e.call(t, "PUT", "/v1/images/-bad", "Bearer "+tokA, strings.NewReader("PLACEHOLDER-body-text"), map[string]string{"X-Image-SHA256": "zz"})
	e.clock.advance(time.Hour)
	e.blobWith(t, "A", "Ticket "+tk.Ticket)

	e.mu.Lock()
	seen := e.seen.String()
	e.mu.Unlock()
	logs := e.logs.String()
	if !strings.Contains(logs, "route=") {
		t.Fatalf("no request lines were captured: %q", logs)
	}
	for what, hay := range map[string]string{"log": logs, "responses": seen} {
		for secret, label := range map[string]string{tk.Ticket: "ticket", tokA: "token", tokB: "token", ticketHash: "ticket hash", tokenhash.Hash(tokA): "token hash", "PLACEHOLDER-body-text": "body"} {
			if strings.Contains(hay, secret) {
				t.Errorf("%s contains the %s", what, label)
			}
		}
	}
}
