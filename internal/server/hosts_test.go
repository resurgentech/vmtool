package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
)

// noLibvirt is a resolver whose local connector fails the test if called.
func noLibvirt(t *testing.T, configured ...string) *hosts.Resolver {
	t.Helper()
	return hosts.NewWith("box", config.RoleHypervisor, configured, func() (app.Manager, func(), error) {
		t.Error("libvirt connector called for a request that must not reach it")
		return nil, nil, errors.New("must not connect")
	})
}

func TestUnknownHost404(t *testing.T) {
	h := handlerFor(&handlers{hosts: noLibvirt(t, "remote1")})
	for _, tc := range []struct {
		method, path, body string
	}{
		{"GET", "/v1/hosts/nope/vms", ""},
		{"GET", "/v1/hosts/nope/vms/a", ""},
		{"DELETE", "/v1/hosts/nope/vms/a", ""},
		{"POST", "/v1/hosts/nope/vms/a/start", ""},
		{"POST", "/v1/hosts/nope/vms/a/cmd", `{"command":["true"]}`},
		{"GET", "/v1/hosts/nope/networks", ""},
		{"GET", "/v1/hosts/nope/bridges", ""},
		{"GET", "/v1/hosts/nope/images", ""},
		{"DELETE", "/v1/hosts/nope/images/a", ""},
		{"GET", "/v1/hosts/nope/pools", ""},
		{"POST", "/v1/hosts/nope/pools", `{"name":"p","path":"/tmp/p"}`},
		{"GET", "/v1/hosts/remote1/vms", ""}, // configured, not served yet
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d, want 404 (%s)", tc.method, tc.path, rec.Code, rec.Body)
			continue
		}
		var e api.Error
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || !strings.Contains(e.Error, "host") || !strings.Contains(e.Error, "box") {
			t.Errorf("%s %s: unclear body %q (%v)", tc.method, tc.path, rec.Body, err)
		}
	}
	// The error names the host asked for and the ones served.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/hosts/nope/vms", nil))
	want := `host \"nope\" is not served by box; known hosts: box`
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body %q lacks %q", rec.Body, want)
	}
}

func TestVersion(t *testing.T) {
	doc := loadSpec(t)
	if doc.Info.Version != "1.0.0" {
		t.Fatalf("spec info.version = %q, want 1.0.0", doc.Info.Version)
	}
	h := handlerFor(&handlers{hosts: noLibvirt(t)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/version", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var v map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v["api_version"] != doc.Info.Version {
		t.Errorf("api_version = %q, want spec info.version %q", v["api_version"], doc.Info.Version)
	}
	sw, err := api.GetSwagger()
	if err != nil || v["api_version"] != sw.Info.Version {
		t.Errorf("api_version %q != embedded spec %q (%v)", v["api_version"], sw.Info.Version, err)
	}
	if v["host"] != "box" {
		t.Errorf("host = %q", v["host"])
	}
	if v["build_version"] == "" {
		t.Errorf("build_version empty")
	}
	if len(v) != 3 {
		t.Errorf("unexpected fields: %v", v)
	}
}

func TestListHosts(t *testing.T) {
	cfg := &config.Config{Name: "ws", Role: config.RoleWorkstation, TokenSHA256: config.Hash(testToken),
		Hosts: map[string]config.Host{"hv1": {Address: "10.0.0.2"}}}
	h := (&Server{Config: cfg}).Handler()
	r := httptest.NewRequest("GET", "/v1/hosts", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got []map[string]any
	if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 ||
		got[0]["name"] != "ws" || got[0]["role"] != "workstation" || got[0]["local"] != true ||
		got[1]["name"] != "hv1" || got[1]["role"] != "hypervisor" || got[1]["local"] != false {
		t.Fatalf("hosts = %v", got)
	}
}
