package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
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
		{"POST", "/v1/hosts/nope/vms/a/exec", `{"command":["true"]}`},
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
	if doc.Info.Version != "2.8.0" {
		t.Fatalf("spec info.version = %q, want 2.8.0", doc.Info.Version)
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

// probeFake stands in for a remote daemon's Manager: it answers Probe.
type probeFake struct {
	app.Manager
	st app.HostStatus
}

func (p probeFake) Probe(context.Context) app.HostStatus { return p.st }

// GET /v1/hosts is the local row and one probed row per configured host,
// each probed with the caller's bearer; a dead host is a row, not a failure.
func TestListHosts(t *testing.T) {
	notAfter := time.Now().Add(10*24*time.Hour + time.Hour).UTC().Truncate(time.Second)
	var mu sync.Mutex
	var tokens []string
	res := hosts.NewWith("ws", config.RoleWorkstation, []string{"hv2", "hv1"}, func() (app.Manager, func(), error) {
		// The local row learns libvirt opens by opening it once (hosts.Status).
		return nil, func() {}, nil
	}).WithRemote(func(host, token string) (app.Manager, func(), error) {
		mu.Lock()
		tokens = append(tokens, host+" "+token)
		mu.Unlock()
		if host == "hv2" {
			return probeFake{st: app.HostStatus{Address: "10.0.0.3:9473", Err: errors.New("dial tcp: connection refused")}}, func() {}, nil
		}
		return probeFake{st: app.HostStatus{Address: "10.0.0.2:9473", Reachable: true, APIVersion: "1.0.0", CertNotAfter: notAfter}}, func() {}, nil
	})
	h := handlerFor(&handlers{hosts: res})
	r := httptest.NewRequest("GET", "/v1/hosts", nil)
	r.Header.Set("Authorization", "Bearer caller-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got []api.HostInfo
	if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "ws" || got[1].Name != "hv1" || got[2].Name != "hv2" {
		t.Fatalf("hosts = %+v", got)
	}
	l := got[0]
	if l.Role != api.Workstation || !l.Local || l.Reachable == nil || !*l.Reachable ||
		l.Address == nil || *l.Address != "local" || l.ApiVersion == nil || *l.ApiVersion == "" || l.CertNotAfter != nil {
		t.Errorf("local row %+v", l)
	}
	v := got[1]
	if v.Role != api.Hypervisor || v.Local || v.Reachable == nil || !*v.Reachable ||
		*v.Address != "10.0.0.2:9473" || *v.ApiVersion != "1.0.0" || v.Error != nil ||
		v.CertNotAfter == nil || !v.CertNotAfter.Equal(notAfter) || v.CertDaysLeft == nil || *v.CertDaysLeft != 10 {
		t.Errorf("hv1 row %+v", v)
	}
	d := got[2]
	if d.Reachable == nil || *d.Reachable || d.Error == nil || *d.Error != "dial tcp: connection refused" || d.CertNotAfter != nil {
		t.Errorf("hv2 row %+v", d)
	}
	sort.Strings(tokens)
	if !reflect.DeepEqual(tokens, []string{"hv1 caller-token", "hv2 caller-token"}) {
		t.Errorf("probes used %q, want the request's bearer", tokens)
	}
	if strings.Contains(rec.Body.String(), "caller-token") {
		t.Error("response contains the token")
	}
}

// With no configured hosts (a hypervisor) only the local row answers, and
// nothing is probed.
func TestListHostsHypervisor(t *testing.T) {
	h := handlerFor(&handlers{hosts: noLibvirt(t)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/hosts", nil))
	var got []api.HostInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got) != 1 || got[0].Name != "box" ||
		got[0].Role != api.Hypervisor || got[0].Reachable == nil || !*got[0].Reachable {
		t.Fatalf("hosts = %s (%v)", rec.Body, err)
	}
}
