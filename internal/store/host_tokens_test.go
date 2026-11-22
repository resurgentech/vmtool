package store

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/internal/tokenhash"
)

const (
	reportRoute       = "PUT /v1/hosts/{host}/inventory"
	hostNetworksRoute = "GET /v1/hosts/{host}/networks"
)

// ---- config -------------------------------------------------------------

func TestDefaultHostTokensPath(t *testing.T) {
	if DefaultHostTokensPath != "/etc/vmtool-store/host_tokens.yaml" {
		t.Fatalf("DefaultHostTokensPath = %q", DefaultHostTokensPath)
	}
}

func TestHostTokensMissingFileMeansNone(t *testing.T) {
	f := newFixture(t) // names a host_tokens_file that does not exist
	c := mustLoad(t, f)
	if len(c.HostTokens) != 0 {
		t.Fatalf("HostTokens = %v, want none", c.HostTokens)
	}
}

func TestHostTokensLoad(t *testing.T) {
	f := newFixture(t)
	f.hostTokens = "hv1: " + tokenhash.Hash(tokH) + "\nPLACEHOLDER-HOST: " + tokenhash.Hash("PLACEHOLDER-not-a-secret-4") + "\n"
	c := mustLoad(t, f)
	if len(c.HostTokens) != 2 || c.HostTokens["hv1"] != tokenhash.Hash(tokH) {
		t.Fatalf("HostTokens = %v", c.HostTokens)
	}
}

func TestHostTokensRefused(t *testing.T) {
	good := tokenhash.Hash(tokH)
	cases := []struct {
		name string
		set  func(*fixture)
		want string
	}{
		{"group readable", func(f *fixture) { f.hostTokens = "hv1: " + good + "\n"; f.hostM = 0o640 }, "host tokens file"},
		{"not a map", func(f *fixture) { f.hostTokens = "- hv1\n- " + tokH + "\n" }, "host tokens file"},
		{"value not a string", func(f *fixture) { f.hostTokens = "hv1: [" + tokH + "]\n" }, "host tokens file"},
		{"host name with a dot", func(f *fixture) { f.hostTokens = "hv1.lab: " + good + "\n" }, "host name"},
		{"host name with a space", func(f *fixture) { f.hostTokens = "\"a b\": " + good + "\n" }, "host name"},
		{"hash not hex", func(f *fixture) { f.hostTokens = "hv1: " + tokH + "\n" }, "64 lowercase hex"},
		{"hash short", func(f *fixture) { f.hostTokens = "hv1: abc\n" }, "64 lowercase hex"},
		{"hash also a user token", func(f *fixture) { f.hostTokens = "hv1: " + tokenhash.Hash(tokA) + "\n" }, "also in the tokens file"},
		{"one hash for two hosts", func(f *fixture) { f.hostTokens = "hv1: " + good + "\nhv2: " + good + "\n" }, "more than one host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			c.set(f)
			mustRefuse(t, f, c.want)
		})
	}
}

// ---- server -------------------------------------------------------------

func hostServer(t *testing.T) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	c := &Config{
		Listen:     "127.0.0.1:8095",
		Tokens:     map[string]string{"alice": tokenhash.Hash(tokA)},
		HostTokens: map[string]string{"hv1": tokenhash.Hash(tokH)},
	}
	s := NewServer(c, slog.New(slog.NewJSONHandler(&logs, nil)), Deps{Blob: blob.NewMemory(), DB: openTestDB(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, &logs
}

func TestWhoamiKind(t *testing.T) {
	ts, logs := hostServer(t)
	for _, c := range []struct {
		tok, name string
		kind      storeapi.WhoamiKind
	}{
		{tokA, "alice", storeapi.WhoamiKindUser},
		{tokH, "host/hv1", storeapi.WhoamiKindHost},
	} {
		resp, body := do(t, "GET", ts.URL+"/v1/whoami", "Bearer "+c.tok, "")
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", c.name, resp.StatusCode)
		}
		var w storeapi.Whoami
		if err := json.Unmarshal([]byte(body), &w); err != nil || w.Name != c.name || w.Kind == nil || *w.Kind != c.kind {
			t.Fatalf("whoami = %q (%v), want name %q kind %q", body, err, c.name, c.kind)
		}
		if strings.Contains(body, c.tok) {
			t.Errorf("whoami body carries the token")
		}
	}
	if !strings.Contains(logs.String(), `"caller":"host/hv1"`) || !strings.Contains(logs.String(), `"caller":"alice"`) {
		t.Errorf("log lacks the callers:\n%s", logs.String())
	}
	for _, tok := range []string{tokA, tokH, tokenhash.Hash(tokH)} {
		if strings.Contains(logs.String(), tok) {
			t.Errorf("log carries %q", tok)
		}
	}
}

// A host token passes only whoami, the report route and the read of its own
// network memberships; every other route in the spec (V3's registry routes
// too) is 401, with the body and log free of it. The memberships route, on a
// host the token is not for, is 403 (here the placeholder host is hv1 and the
// token is hv1's, so it answers 200 or 500 with no database, never 401).
func TestHostTokenAllowList(t *testing.T) {
	ts, logs := hostServer(t)
	var seen int
	for op, public := range specOps(t) {
		if public {
			continue
		}
		seen++
		method, p, _ := strings.Cut(op, " ")
		resp, body := do(t, method, ts.URL+concrete(p), "Bearer "+tokH, "")
		allowed := op == "GET /v1/whoami" || op == reportRoute || op == hostNetworksRoute
		if allowed && resp.StatusCode == 401 {
			t.Errorf("%s with a host token = 401, want it accepted", op)
		}
		if !allowed && resp.StatusCode != 401 {
			t.Errorf("%s with a host token = %d, want 401", op, resp.StatusCode)
		}
		if strings.Contains(body, tokH) {
			t.Errorf("%s echoes the host token", op)
		}
	}
	if seen < 26 {
		t.Fatalf("only %d protected operations in the spec", seen)
	}
	// Unknown paths and tickets-style misuse learn nothing.
	if resp, _ := do(t, "GET", ts.URL+"/v1/nope", "Bearer "+tokH, ""); resp.StatusCode != 401 {
		t.Errorf("unknown path with a host token = %d, want 401", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/v1/images/PLACEHOLDER-name/blob", "Ticket "+tokH, ""); resp.StatusCode != 401 {
		t.Errorf("host token as a ticket = %d, want 401", resp.StatusCode)
	}
	if strings.Contains(logs.String(), tokH) || strings.Contains(logs.String(), tokenhash.Hash(tokH)) {
		t.Errorf("log carries the host token or its hash")
	}
}

// The report route arrives in N3; until then the allow-list names it, so
// authenticate accepts a host token on that pattern and on no other write.
func TestHostTokenAllowListNamesReportRoute(t *testing.T) {
	c := &Config{Tokens: map[string]string{"alice": tokenhash.Hash(tokA)}, HostTokens: map[string]string{"hv1": tokenhash.Hash(tokH)}}
	s := NewServer(c, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), Deps{})
	req := func() *http.Request {
		r := httptest.NewRequest("PUT", "/v1/hosts/hv1/inventory", nil)
		r.Header.Set("Authorization", "Bearer "+tokH)
		return r
	}
	if name, ok := s.authenticate(req(), reportRoute); !ok || name != "host/hv1" {
		t.Errorf("host token on the report route = %q, %v", name, ok)
	}
	if name, ok := s.authenticate(req(), hostNetworksRoute); !ok || name != "host/hv1" {
		t.Errorf("host token on the networks route = %q, %v", name, ok)
	}
	for _, pat := range []string{"", "PUT /v1/hosts/{host}/networks", "POST /v1/hosts/{host}/networks", "GET /v1/networks", "GET /v1/hosts/{host}/inventory", "PUT /v1/hosts/{host}/inventory/x", "GET /v1/inventory", "GET /v1/images"} {
		if _, ok := s.authenticate(req(), pat); ok {
			t.Errorf("host token accepted on %q", pat)
		}
	}
}

// A user token still passes everything it did before.
func TestUserTokenUnchangedWithHostTokens(t *testing.T) {
	ts, _ := hostServer(t)
	if resp, _ := do(t, "GET", ts.URL+"/v1/images", "Bearer "+tokA, ""); resp.StatusCode != 200 {
		t.Fatalf("user token on listImages = %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/v1/images", "Bearer PLACEHOLDER-wrong", ""); resp.StatusCode != 401 {
		t.Fatalf("wrong token = %d", resp.StatusCode)
	}
}
