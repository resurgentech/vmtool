package store

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/internal/tokenhash"
)

// ---- the setting ----------------------------------------------------------

func TestPathPrefixAccepted(t *testing.T) {
	for _, p := range []string{"", "/a", "/a/b", "/v.1_~-", "/vmstore"} {
		t.Run(p, func(t *testing.T) {
			f := newFixture(t)
			if p != "" {
				f.extra = "path_prefix: \"" + p + "\"\n"
			}
			if c := mustLoad(t, f); c.PathPrefix != p {
				t.Fatalf("PathPrefix = %q, want %q", c.PathPrefix, p)
			}
		})
	}
}

func TestPathPrefixRefused(t *testing.T) {
	for _, p := range []string{"a", "/", "/a/", "//a", "/a//b", "/.", "/a/..", "/a b", "/%61", "/a/./b", "/a..b"} {
		t.Run(p, func(t *testing.T) {
			f := newFixture(t)
			f.extra = "path_prefix: \"" + p + "\"\n"
			mustRefuse(t, f, "path_prefix")
		})
	}
}

// ---- the server under a prefix --------------------------------------------

const testPrefix = "/vmstore"

func prefixServer(t *testing.T) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	c := &Config{
		Listen:     "127.0.0.1:8095",
		PathPrefix: testPrefix,
		Tokens:     map[string]string{"alice": tokenhash.Hash(tokA)},
		HostTokens: map[string]string{"hv1": tokenhash.Hash(tokH)},
	}
	s := NewServer(c, slog.New(slog.NewJSONHandler(&logs, nil)), Deps{Blob: blob.NewMemory(), DB: openTestDB(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, &logs
}

func TestPrefixVersionPublic(t *testing.T) {
	ts, _ := prefixServer(t)
	resp, body := do(t, "GET", ts.URL+testPrefix+"/version", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var v storeapi.Version
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Version != "1.4.0" {
		t.Fatalf("body %q err %v", body, err)
	}
}

func TestPrefixWhoami(t *testing.T) {
	ts, _ := prefixServer(t)
	if resp, _ := do(t, "GET", ts.URL+testPrefix+"/v1/whoami", "", ""); resp.StatusCode != 401 {
		t.Fatalf("whoami without token = %d, want 401", resp.StatusCode)
	}
	for tok, want := range map[string]string{tokA: "alice", tokH: "host/hv1"} {
		resp, body := do(t, "GET", ts.URL+testPrefix+"/v1/whoami", "Bearer "+tok, "")
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", want, resp.StatusCode)
		}
		var w storeapi.Whoami
		if err := json.Unmarshal([]byte(body), &w); err != nil || w.Name != want {
			t.Fatalf("whoami = %q (%v), want %q", body, err, want)
		}
	}
}

// Outside the prefix nothing answers but 404 with the store's error body,
// with or without a token, and the request reaches no route.
func TestPrefixOutsideIs404(t *testing.T) {
	ts, logs := prefixServer(t)
	for _, p := range []string{"/version", "/v1/whoami", testPrefix, "/vmstorex/version", "/", "/vmstorex/v1/whoami", "/v1/images"} {
		for _, auth := range []string{"", "Bearer " + tokA} {
			resp, body := do(t, "GET", ts.URL+p, auth, "")
			if resp.StatusCode != 404 {
				t.Errorf("GET %s (auth %t) = %d, want 404", p, auth != "", resp.StatusCode)
			}
			if resp.Header.Get("WWW-Authenticate") != "" {
				t.Errorf("GET %s asks for a token", p)
			}
			var e storeapi.Error
			if err := json.Unmarshal([]byte(body), &e); err != nil || e.Error != "not found" {
				t.Errorf("GET %s body %q", p, body)
			}
		}
	}
	// an encoded prefix is not the prefix
	if resp, _ := do(t, "GET", ts.URL+"/%76mstore/version", "", ""); resp.StatusCode != 404 {
		t.Errorf("encoded prefix = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(logs.String(), tokA) {
		t.Error("log carries the token")
	}
	if strings.Contains(logs.String(), `"caller":"alice"`) {
		t.Error("a request outside the prefix was authenticated")
	}
}

// A host token keeps its three-route allow-list under the prefix, and
// another host's networks are 403.
func TestPrefixHostTokenAllowList(t *testing.T) {
	ts, _ := prefixServer(t)
	var seen int
	for op, public := range specOps(t) {
		if public {
			continue
		}
		seen++
		method, p, _ := strings.Cut(op, " ")
		resp, _ := do(t, method, ts.URL+testPrefix+concrete(p), "Bearer "+tokH, "")
		allowed := op == "GET /v1/whoami" || op == reportRoute || op == hostNetworksRoute
		if allowed && resp.StatusCode == 401 {
			t.Errorf("%s with a host token = 401, want it accepted", op)
		}
		if !allowed && resp.StatusCode != 401 {
			t.Errorf("%s with a host token = %d, want 401", op, resp.StatusCode)
		}
	}
	if seen < 3 {
		t.Fatalf("only %d operations", seen)
	}
	if resp, _ := do(t, "GET", ts.URL+testPrefix+"/v1/hosts/hv2/networks", "Bearer "+tokH, ""); resp.StatusCode != 403 {
		t.Errorf("another host's networks = %d, want 403", resp.StatusCode)
	}
}

// A ticket downloads its image once under the prefix; the second use and an
// encoded path under the prefix are refused.
func TestPrefixTicketOneUse(t *testing.T) {
	e := &env{blob: newTrackBlob(), db: openTestDB(t), logs: &lockedBuf{}, clock: &clock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}}
	c := &Config{
		Listen:     "127.0.0.1:8095",
		PathPrefix: testPrefix,
		Tokens:     map[string]string{"alice": tokenhash.Hash(tokA)},
	}
	e.srv = NewServer(c, slog.New(slog.NewTextHandler(e.logs, nil)), Deps{Blob: e.blob, DB: e.db, Now: e.clock.now})
	e.ts = httptest.NewServer(e.srv.Handler())
	t.Cleanup(e.ts.Close)

	body := payload(60, 1<<20)
	resp, b := e.call(t, "PUT", testPrefix+"/v1/images/A", "Bearer "+tokA, bytes.NewReader(body), map[string]string{"X-Image-SHA256": sum(body)})
	if resp.StatusCode != 201 {
		t.Fatalf("push = %d %s", resp.StatusCode, b)
	}
	mint := func() storeapi.Ticket {
		resp, b := e.call(t, "POST", testPrefix+"/v1/images/A/tickets", "Bearer "+tokA, nil, nil)
		if resp.StatusCode != 201 {
			t.Fatalf("mint = %d %s", resp.StatusCode, b)
		}
		var tk storeapi.Ticket
		if err := json.Unmarshal(b, &tk); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	tk := mint()
	resp, b = e.call(t, "GET", testPrefix+"/v1/images/A/blob", "Ticket "+tk.Ticket, nil, nil)
	if resp.StatusCode != 200 || !bytes.Equal(b, body) {
		t.Fatalf("first use = %d", resp.StatusCode)
	}
	if resp, _ := e.call(t, "GET", testPrefix+"/v1/images/A/blob", "Ticket "+tk.Ticket, nil, nil); resp.StatusCode != 401 {
		t.Fatalf("second use = %d, want 401", resp.StatusCode)
	}
	// an encoded name under the prefix is refused, and does not spend the ticket
	tk = mint()
	if resp, _ := e.call(t, "GET", testPrefix+"/v1/images/%41/blob", "Ticket "+tk.Ticket, nil, nil); resp.StatusCode != 401 {
		t.Errorf("encoded name with a ticket = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.call(t, "GET", testPrefix+"/v1/images/../v1/images/A/blob", "Ticket "+tk.Ticket, nil, nil); resp.StatusCode == 200 {
		t.Errorf("unclean path with a ticket = %d", resp.StatusCode)
	}
	if resp, _ := e.call(t, "GET", testPrefix+"/v1/images/A/blob", "Ticket "+tk.Ticket, nil, nil); resp.StatusCode != 200 {
		t.Errorf("ticket spent by a refused path: %d", resp.StatusCode)
	}
	if !strings.Contains(e.logs.String(), "ticket:alice") {
		t.Errorf("log lacks the ticket caller:\n%s", e.logs.String())
	}
}

// Under a prefix the store emits no redirect: an unclean path below it is
// 404 before auth, where the mux would otherwise answer a 301 whose Location
// lacks the prefix.
func TestPrefixUncleanPathNoRedirect(t *testing.T) {
	ts, _ := prefixServer(t)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, p := range []string{"/v1/../v1/whoami", "/v1//whoami", "/./version", "/v1/whoami/."} {
		for _, auth := range []string{"", "Bearer " + tokA} {
			req, err := http.NewRequest("GET", ts.URL+testPrefix+p, nil)
			if err != nil {
				t.Fatal(err)
			}
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			resp, err := noFollow.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 404 || resp.Header.Get("Location") != "" {
				t.Errorf("GET %s%s (auth %t) = %d Location %q, want 404 and none", testPrefix, p, auth != "", resp.StatusCode, resp.Header.Get("Location"))
			}
		}
	}
}

// The log line under a prefix names the spec route, as without one.
func TestPrefixLogRoute(t *testing.T) {
	ts, logs := prefixServer(t)
	do(t, "GET", ts.URL+testPrefix+"/v1/whoami", "Bearer "+tokA, "")
	if !strings.Contains(logs.String(), `"route":"/v1/whoami"`) || !strings.Contains(logs.String(), `"caller":"alice"`) {
		t.Errorf("log line:\n%s", logs.String())
	}
}
