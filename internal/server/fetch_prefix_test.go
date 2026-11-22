package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/store"
	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/store/db"
	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/internal/tokenhash"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// A daemon whose store_url carries a path fetches with a ticket from the real
// store handler (in-process, httptest TLS) serving under that path; the
// ticket is spent by the fetch.
func TestFetchImageRealStoreUnderPathPrefix(t *testing.T) {
	const userTok = "PLACEHOLDER-not-a-secret-2"
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(context.Background(), filepath.Join(dir, "store.db"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	cfg := &store.Config{Listen: "127.0.0.1:8095", PathPrefix: "/vmstore", Tokens: map[string]string{"alice": tokenhash.Hash(userTok)}}
	real := store.NewServer(cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), store.Deps{Blob: blob.NewMemory(), DB: d}).Handler()
	s := newTestStore(t, real.ServeHTTP)

	call := func(method, p, auth string, body []byte, hdr map[string]string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, s.srv.URL+p, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", auth)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := s.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	data := randomBytes(t, 200000)
	if code, b := call("PUT", "/vmstore/v1/images/base.qcow2", "Bearer "+userTok, data, map[string]string{"X-Image-SHA256": hexSum(data)}); code != 201 {
		t.Fatalf("push = %d %s", code, b)
	}
	code, b := call("POST", "/vmstore/v1/images/base.qcow2/tickets", "Bearer "+userTok, nil, nil)
	if code != 201 {
		t.Fatalf("mint = %d %s", code, b)
	}
	var tk storeapi.Ticket
	if err := json.Unmarshal(b, &tk); err != nil {
		t.Fatal(err)
	}

	f := newFetchFixture(t, s)
	f.h.fetchURL = s.srv.URL + "/vmstore"
	end := last(t, events(t, f.fetch("base.qcow2", fetchBody(tk.Sha256, int(tk.Size), tk.Ticket))))
	if end.Event != api.FetchImageEventEventDone || end.Image == nil || end.Image.Sha256 == nil || *end.Image.Sha256 != hexSum(data) {
		t.Fatalf("terminal %+v", end)
	}
	got, err := os.ReadFile(filepath.Join(f.pm.dirs[vmtool.HypervisorPool], "base.qcow2"))
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("stored file: %v", err)
	}
	rs := s.requests()
	if p := rs[len(rs)-1].URL.EscapedPath(); p != "/vmstore/v1/images/base.qcow2/blob" {
		t.Errorf("daemon asked for %s", p)
	}
	// The ticket was spent by the daemon's download.
	if code, _ := call("GET", "/vmstore/v1/images/base.qcow2/blob", "Ticket "+tk.Ticket, nil, nil); code != 401 {
		t.Errorf("ticket reuse = %d, want 401", code)
	}
}
