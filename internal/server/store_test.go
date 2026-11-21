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
	// reqs counts every request that reached the store (whoami included);
	// delists counts POST .../delist.
	reqs, delists atomic.Int32
	// notes counts PUT .../notes.
	notes atomic.Int32
	// invQuery is the last GET /v1/inventory request URI; invs counts them.
	invQuery atomic.Value
	invs     atomic.Int32
}

func newSrvStore(t *testing.T) *srvStore {
	t.Helper()
	s := &srvStore{}
	s.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s.reqs.Add(1)
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
		if r.Method == http.MethodGet && r.URL.Path == "/v1/inventory" {
			s.invs.Add(1)
			s.invQuery.Store(r.URL.RequestURI())
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hosts": []any{map[string]any{"name": "hv1", "last_report": "2026-11-21T01:02:03Z", "daemon_version": "2.6.0", "live": 1, "gone": 1}},
				"vms": []any{
					map[string]any{"host": "hv1", "name": "vm1", "uuid": "00000000-0000-4000-8000-000000000001", "state": "running",
						"vcpus": 2, "memory": 4, "ip": "10.0.0.5", "autostart": true, "firmware": "efi",
						"disks":            []any{map[string]any{"target": "vda", "pool": "nvme", "capacity_bytes": 21474836480, "kind": "thin"}},
						"origin":           map[string]any{"image": "img1", "image_sha256": strings.Repeat("a", 64), "clone": "thin", "pool": "nvme", "created": "2026-11-20T00:00:00Z"},
						"disks_unreadable": true,
						"comment":          "placeholder comment", "first_seen": "2026-11-20T00:00:00Z", "last_seen": "2026-11-21T01:02:03Z", "has_credentials": true},
					map[string]any{"host": "hv1", "name": "vm2", "uuid": "00000000-0000-4000-8000-000000000002", "state": "shutoff",
						"vcpus": 1, "memory": 1, "ip": "", "autostart": false, "firmware": "bios", "disks": []any{}, "comment": "",
						"first_seen": "2026-11-20T00:00:00Z", "last_seen": "2026-11-20T00:00:00Z", "gone_since": "2026-11-21T01:02:03Z",
						"credentials_kept_until": "2026-11-28T01:02:03Z", "has_credentials": false},
				},
				"machines": []any{map[string]any{"name": "nuc1", "kind": "metal"}},
			})
			return
		}
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/notes") {
			s.notes.Add(1)
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["comment"] == "gone" {
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "the VM is gone from the inventory"})
				return
			}
			c, _ := in["comment"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"host": "hv1", "name": "vm1", "uuid": "00000000-0000-4000-8000-000000000001",
				"comment": c, "origin": map[string]any{"image": "img1", "clone": "thin"}})
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delist") {
			s.delists.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"uuid": "00000000-0000-4000-8000-000000000003",
				"had_credentials": true, "credentials_kept_until": "2026-11-28T03:04:05Z"})
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
	if doc.Info.Version != "2.6.0" {
		t.Errorf("API version %s: want 2.6.0", doc.Info.Version)
	}
}
