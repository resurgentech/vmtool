package store

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/storeapi"
	"github.com/resurgentech/vmtool/internal/tokenhash"
)

func testServer(t *testing.T) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	c := &Config{
		Listen: "127.0.0.1:8095",
		Tokens: map[string]string{
			"alice": tokenhash.Hash(tokA),
			"bob":   tokenhash.Hash(tokB),
		},
	}
	s := NewServer(c, slog.New(slog.NewJSONHandler(&logs, nil)), Deps{Blob: blob.NewMemory(), DB: openTestDB(t)})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, &logs
}

func do(t *testing.T, method, url, auth string, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp, b.String()
}

// specOps lists every "METHOD /path" in the embedded spec and whether it is public.
func specOps(t *testing.T) map[string]bool {
	t.Helper()
	doc, err := storeapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for p, item := range doc.Paths.Map() {
		for m, op := range item.Operations() {
			out[strings.ToUpper(m)+" "+p] = isPublic(doc, op)
		}
	}
	return out
}

func concrete(p string) string {
	p = strings.ReplaceAll(p, "{name}", "PLACEHOLDER-name")
	p = strings.ReplaceAll(p, "{mac}", "52:54:00:00:00:01")
	return strings.ReplaceAll(p, "{host}", "hv1")
}

func TestAuthEveryOperation(t *testing.T) {
	ts, _ := testServer(t)
	ops := specOps(t)
	if len(ops) != 32 {
		t.Fatalf("only %d operations in the spec", len(ops))
	}
	for op, public := range ops {
		method, p, _ := strings.Cut(op, " ")
		u := ts.URL + concrete(p)
		if public {
			if op != "GET /version" {
				t.Errorf("%s is public; only GET /version may be", op)
			}
			if resp, _ := do(t, method, u, "", ""); resp.StatusCode != 200 {
				t.Errorf("%s without token = %d, want 200", op, resp.StatusCode)
			}
			continue
		}
		for name, auth := range map[string]string{
			"none":         "",
			"wrong":        "Bearer PLACEHOLDER-wrong",
			"wrong scheme": "Basic " + tokA,
			"ticket":       "Ticket " + tokA,
			"empty bearer": "Bearer ",
		} {
			resp, body := do(t, method, u, auth, "")
			if resp.StatusCode != 401 {
				t.Errorf("%s with %s token = %d, want 401", op, name, resp.StatusCode)
			}
			if strings.Contains(body, tokA) {
				t.Errorf("%s echoes the token", op)
			}
		}
	}
}

func TestUnknownPathNeedsToken(t *testing.T) {
	ts, _ := testServer(t)
	if resp, _ := do(t, "GET", ts.URL+"/v1/nope", "", ""); resp.StatusCode != 401 {
		t.Fatalf("unknown path without token = %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", ts.URL+"/v1/nope", "Bearer "+tokA, ""); resp.StatusCode != 404 {
		t.Fatalf("unknown path with token = %d", resp.StatusCode)
	}
}

func TestVersionPublic(t *testing.T) {
	ts, _ := testServer(t)
	resp, body := do(t, "GET", ts.URL+"/version", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var v storeapi.Version
	if err := json.Unmarshal([]byte(body), &v); err != nil || v.Version != "1.4.0" {
		t.Fatalf("body %q err %v", body, err)
	}
}

func TestWhoamiReturnsCallerName(t *testing.T) {
	ts, _ := testServer(t)
	for tok, want := range map[string]string{tokA: "alice", tokB: "bob"} {
		resp, body := do(t, "GET", ts.URL+"/v1/whoami", "Bearer "+tok, "")
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var w storeapi.Whoami
		if err := json.Unmarshal([]byte(body), &w); err != nil || w.Name != want {
			t.Fatalf("whoami = %q (%v), want %q", body, err, want)
		}
	}
	// scheme is case-insensitive, like vmtool's own server
	if resp, _ := do(t, "GET", ts.URL+"/v1/whoami", "bearer "+tokA, ""); resp.StatusCode != 200 {
		t.Fatalf("lowercase scheme = %d", resp.StatusCode)
	}
}

func TestLogLine(t *testing.T) {
	ts, logs := testServer(t)
	do(t, "GET", ts.URL+"/v1/images/PLACEHOLDER-name?secret=PLACEHOLDER-query", "Bearer "+tokA, `{"password":"PLACEHOLDER-not-a-secret-2"}`)
	do(t, "GET", ts.URL+"/v1/whoami", "Bearer PLACEHOLDER-wrong", "")
	do(t, "GET", ts.URL+"/version", "", "")

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d log lines, want 3:\n%s", len(lines), logs.String())
	}
	want := []map[string]any{
		{"method": "GET", "route": "/v1/images/{name}", "status": 404.0, "caller": "alice"},
		{"method": "GET", "route": "/v1/whoami", "status": 401.0, "caller": "-"},
		{"method": "GET", "route": "/version", "status": 200.0, "caller": "-"},
	}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		var keys []string
		for k := range rec {
			switch k {
			case "time", "level", "msg":
			default:
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != "caller,duration,method,route,status" {
			t.Errorf("line %d fields = %v", i, keys)
		}
		for k, v := range want[i] {
			if rec[k] != v {
				t.Errorf("line %d %s = %v, want %v", i, k, rec[k], v)
			}
		}
		if _, ok := rec["duration"].(float64); !ok {
			t.Errorf("line %d duration = %v", i, rec["duration"])
		}
	}
	for _, leak := range []string{tokA, tokB, "PLACEHOLDER-query", "PLACEHOLDER-name", "PLACEHOLDER-wrong", "Bearer", tokenhash.Hash(tokA)} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("log contains %q", leak)
		}
	}
}

// Every route the handler registers is in store.yaml and vice versa.
type recMux struct{ routes map[string]bool }

func (m *recMux) HandleFunc(pattern string, _ func(http.ResponseWriter, *http.Request)) {
	if m.routes == nil {
		m.routes = map[string]bool{}
	}
	m.routes[pattern] = true
}
func (m *recMux) ServeHTTP(http.ResponseWriter, *http.Request) {}

func TestSpecParity(t *testing.T) {
	rec := &recMux{}
	storeapi.HandlerFromMux(storeapi.NewStrictHandler(&handlers{}, nil), rec)
	spec := map[string]bool{}
	for op := range specOps(t) {
		method, p, _ := strings.Cut(op, " ")
		spec[method+" "+path.Clean("/"+p)] = true
	}
	var missing, extra []string
	for k := range rec.routes {
		if !spec[k] {
			extra = append(extra, k)
		}
	}
	for k := range spec {
		if !rec.routes[k] {
			missing = append(missing, k)
		}
	}
	if len(missing)+len(extra) > 0 {
		t.Fatalf("parity mismatch\nmissing from mux: %v\nextra on mux: %v", missing, extra)
	}
}

// The spec, not the code, says which routes are public.
func TestOnlyVersionIsPublic(t *testing.T) {
	doc, err := storeapi.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	var pub []string
	for p, item := range doc.Paths.Map() {
		for m, op := range item.Operations() {
			if isPublic(doc, op) {
				pub = append(pub, m+" "+p)
			}
		}
	}
	if len(pub) != 1 || pub[0] != "GET /version" {
		t.Fatalf("public operations = %v", pub)
	}
	var _ *openapi3.T = doc
}
