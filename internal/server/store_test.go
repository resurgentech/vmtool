package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/spec"
)

const (
	srvStoreTok = "PLACEHOLDER-not-a-secret-1"
	warnHeader  = "X-Vmtool-Warnings"
	srvLine     = "127.0.0.1 unreachable: using this machine's .machines/ and image_dir"
)

type srvStore struct {
	ts     *httptest.Server
	whoami atomic.Int32
	down   atomic.Bool
}

func newSrvStore(t *testing.T) *srvStore {
	t.Helper()
	s := &srvStore{}
	s.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if s.down.Load() {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/v1/whoami" {
			s.whoami.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+srvStoreTok {
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"name": "jared"})
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// storeServer is a loopback service whose config.yml names the store at url
// (empty: none), with the store token in the environment.
func storeServer(t *testing.T, s *srvStore, storeToken string) *httptest.Server {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv(config.ServerTokenEnv, storeToken)
	cfg := &config.Config{Dir: t.TempDir(), TokenSHA256: config.Hash(testToken)}
	srv := &Server{Config: cfg, PlaybookDir: "playbooks"}
	if s != nil {
		cfg.Server.URL = s.ts.URL
		pool := s.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
		srv.StoreOptions = []storeclient.Option{storeclient.WithRootCAs(pool)}
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func authedGet(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

// Each request decides the mode itself, at its start, when its operation uses
// credentials or base images; the one line rides in the response's warnings.
func TestRequestWarnsWhenStoreUnreachable(t *testing.T) {
	s := newSrvStore(t)
	ts := storeServer(t, s, srvStoreTok)

	res := authedGet(t, ts, "/v1/metal")
	if res.StatusCode != 200 || res.Header.Get(warnHeader) != "" {
		t.Fatalf("reachable: %d, %s %q", res.StatusCode, warnHeader, res.Header.Get(warnHeader))
	}
	if s.whoami.Load() != 1 {
		t.Fatalf("whoami %d", s.whoami.Load())
	}

	s.down.Store(true)
	res = authedGet(t, ts, "/v1/metal")
	if res.StatusCode != 200 {
		t.Fatalf("status %d: the request still succeeds in local mode", res.StatusCode)
	}
	if got := res.Header.Values(warnHeader); len(got) != 1 || got[0] != srvLine {
		t.Fatalf("%s = %q, want exactly [%q]", warnHeader, got, srvLine)
	}

	// The next request decides again: the store is back, no warning.
	s.down.Store(false)
	res = authedGet(t, ts, "/v1/metal")
	if res.Header.Get(warnHeader) != "" {
		t.Fatalf("stale warning %q", res.Header.Get(warnHeader))
	}
	if s.whoami.Load() != 2 { // one while down never reached whoami's counter
		t.Fatalf("whoami %d, want 2 (first and third request)", s.whoami.Load())
	}
}

func TestRequestWrongStoreTokenWarns(t *testing.T) {
	s := newSrvStore(t)
	ts := storeServer(t, s, "PLACEHOLDER-wrong")
	res := authedGet(t, ts, "/v1/metal")
	if res.Header.Get(warnHeader) != srvLine || res.StatusCode != 200 {
		t.Fatalf("%d %q", res.StatusCode, res.Header.Get(warnHeader))
	}
	for k, v := range res.Header {
		if strings.Contains(strings.Join(v, ""), "PLACEHOLDER-wrong") {
			t.Fatalf("header %s carries the store token", k)
		}
	}
}

func TestRequestMissingStoreTokenWarns(t *testing.T) {
	s := newSrvStore(t)
	ts := storeServer(t, s, "")
	res := authedGet(t, ts, "/v1/metal")
	want := "127.0.0.1: no VMTOOL_SERVER_TOKEN in environment or .env: using this machine's .machines/ and image_dir"
	if res.Header.Get(warnHeader) != want || s.whoami.Load() != 0 {
		t.Fatalf("%q whoami %d", res.Header.Get(warnHeader), s.whoami.Load())
	}
}

// Operations that use neither credentials nor images never probe; with no
// server: block nothing is probed or warned.
func TestRequestsThatNeedNoStoreDoNotProbe(t *testing.T) {
	s := newSrvStore(t)
	ts := storeServer(t, s, srvStoreTok)
	s.down.Store(true)
	res := authedGet(t, ts, "/v1/playbooks")
	if res.Header.Get(warnHeader) != "" || s.whoami.Load() != 0 {
		t.Fatalf("%q whoami %d", res.Header.Get(warnHeader), s.whoami.Load())
	}
	// A 401 is not a store request either.
	ts2 := storeServer(t, s, srvStoreTok)
	req, _ := http.NewRequest(http.MethodGet, ts2.URL+"/v1/metal", nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 401 || r.Header.Get(warnHeader) != "" {
		t.Fatalf("unauthenticated: %d %q", r.StatusCode, r.Header.Get(warnHeader))
	}

	ts3 := storeServer(t, nil, srvStoreTok)
	res = authedGet(t, ts3, "/v1/metal")
	if res.StatusCode != 200 || res.Header.Get(warnHeader) != "" {
		t.Fatalf("no server: %d %q", res.StatusCode, res.Header.Get(warnHeader))
	}
}

// The hand list of operations that decide the mode. Every name must be an
// operation in the spec.
func TestStoreOperationsAreSpecOperations(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(spec.OpenAPIYAML)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			ids[op.OperationID] = true
		}
	}
	for op := range storeOperations {
		if !ids[op] {
			t.Errorf("storeOperations lists %q, which is not in the spec", op)
		}
	}
	for _, want := range []string{"createVM", "deleteVM", "execVM", "runPlaybook", "resizeDisk",
		"listMachines", "addMachine", "getMachine", "removeMachine", "rebootMachine", "execMachine", "runMachinePlaybook"} {
		if !storeOperations[want] {
			t.Errorf("%s does not decide the store mode", want)
		}
	}
	for _, not := range []string{"listVMs", "getVM", "startVM", "listPlaybooks", "getVersion", "openVMTunnel"} {
		if storeOperations[not] {
			t.Errorf("%s decides the store mode", not)
		}
	}
}

// The header is part of the contract, so the spec says so.
func TestSpecDocumentsWarningsHeader(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(spec.OpenAPIYAML)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Info.Description, warnHeader) {
		t.Errorf("info.description does not document %s", warnHeader)
	}
	if doc.Info.Version != "2.5.0" {
		t.Errorf("API version %s: want 2.5.0", doc.Info.Version)
	}
}
