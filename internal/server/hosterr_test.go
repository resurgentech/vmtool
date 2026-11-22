package server

import (
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/remote"
	"github.com/resurgentech/vmtool/spec"
)

// forwardBodies is every per-host operation a workstation forwards to a
// configured host's daemon, with a body that gets past validation to the
// forwarded call. The test below drives each one against a host that is down.
var forwardBodies = map[string]string{
	"listVMs":            "",
	"createVM":           `{"name":"web","image":"ubuntu.qcow2"}`,
	"getVM":              "",
	"deleteVM":           "",
	"startVM":            "",
	"stopVM":             "",
	"resumeVM":           "",
	"poweroffVM":         "",
	"rebootVM":           "",
	"setAutostart":       `{"enabled":true}`,
	"execVM":             `{"command":["true"]}`,
	"runPlaybook":        `{"playbook":"setup.yml"}`,
	"migrateDisk":        `{"pool":"fast"}`,
	"resizeDisk":         `{"size":40}`,
	"addDisk":            `{"size":10}`,
	"listNetworks":       "",
	"createNetwork":      `{"name":"lab","subnet":"10.77.1.0/24"}`,
	"getNetwork":         "",
	"deleteNetwork":      "",
	"createReservation":  `{"vm":"web"}`,
	"deleteReservation":  "",
	"setNetworkPeers":    `{"lan_ip":"192.0.2.1","lan_dev":"eth0","peers":["192.0.2.2"]}`,
	"deleteNetworkPeers": "",
	"getUnderlay":        "",
	"listBridges":        "",
	"listImages":         "",
	"deleteImage":        "",
	"fetchImage":         `{"sha256":"0000000000000000000000000000000000000000000000000000000000000000","size":1,"ticket":"0123456789abcdef0123"}`,
	"getImageManifest":   "",
	"getImageBlob":       "",
	"listPools":          "",
	"createPool":         `{"name":"p","path":"/var/lib/p"}`,
	"listTemplates":      "",
	"createTemplate":     `{"name":"base","from_vm":"web"}`,
	"deleteTemplate":     "",
	"exportTemplate":     "",
	"getImagePath":       "",
	"createVolume":       `{"pool":"p","name":"v.qcow2","size_gb":1}`,
	"cloneVolume":        `{"base_path":"/pool/a.qcow2","new_name":"b"}`,
	"resizeVolume":       `{"path":"/pool/v.qcow2","size_bytes":1048576}`,
	"defineVM":           `{"vcpus":1,"memory_gib":1,"disk_path":"/pool/web.qcow2","network":{"type":"nat"}}`,
	"deleteVMDefinition": "",
	"attachInterface":    `{"network":"lab","mac":"52:54:00:00:00:01"}`,
	"detachInterface":    "",
	"waitVMIP":           `{}`,
	"resizeVMVolume":     `{"size_gb":2}`,
}

// forwardSkipped are the per-host operations that do not forward a call to
// the host's daemon, with the reason.
var forwardSkipped = map[string]string{
	"openVMTunnel":  "the workstation refuses a tunnel to a remote host's VM (TestTunnelRefusesRemoteHost)",
	"openVMConsole": "a local ssh on a pty, not a forwarded call",
	"uploadImage":   "served by hypervisor daemons only; a workstation answers 403",
	"setVMComment":  "kept in the store, not on the host: a store that does not answer is 503",
}

// hostDownWorkstation is the real workstation chain whose one configured host
// is served by a real remote.Manager pointed at url.
func hostDownWorkstation(t *testing.T, url string, pool *x509.CertPool) http.Handler {
	t.Helper()
	res := hosts.NewWith("ws", config.RoleWorkstation, []string{"veritech01"}, func() (app.Manager, func(), error) {
		t.Error("workstation libvirt opened")
		return nil, func() {}, nil
	}).WithRemote(func(host, tok string) (app.Manager, func(), error) {
		m, err := remote.New(host, url, tok, remote.WithRootCAs(pool), remote.WithTimeouts(150*time.Millisecond, 150*time.Millisecond))
		if err != nil {
			return nil, nil, err
		}
		return m, m.Close, nil
	})
	h := &handlers{hosts: res, role: config.RoleWorkstation, tunnelExe: stubTunnelExe}
	return chain(h, func(next http.Handler) http.Handler {
		return requireBearer(config.Hash(testToken), publicRoutes(), next)
	})
}

// forwardOps lists the per-host operations of the spec as opID -> request.
func forwardOps(t *testing.T) map[string][2]string {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(spec.OpenAPIYAML)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]string{}
	repl := strings.NewReplacer("{host}", "veritech01", "{name}", "web", "{mac}", "52:54:00:00:00:01")
	for p, item := range doc.Paths.Map() {
		if !strings.HasPrefix(p, "/v1/hosts/{host}") {
			continue
		}
		for method, op := range item.Operations() {
			out[op.OperationID] = [2]string{method, repl.Replace(p)}
		}
	}
	return out
}

// Every operation a workstation forwards answers 502 host_unreachable when the
// host refuses the connection, and 504 host_timeout when it does not answer.
func TestForwardedOperationsReportAHostThatIsDown(t *testing.T) {
	t.Chdir(t.TempDir())
	ops := forwardOps(t)
	var names []string
	for id := range ops {
		_, fwd := forwardBodies[id]
		_, skip := forwardSkipped[id]
		if fwd == skip {
			t.Errorf("operation %s must be in exactly one of forwardBodies and forwardSkipped", id)
		}
		names = append(names, id)
	}
	for id := range forwardBodies {
		if _, ok := ops[id]; !ok {
			t.Errorf("forwardBodies names %s, which is not a per-host operation", id)
		}
	}
	sort.Strings(names)

	// refused: a port nothing listens on.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedURL := "https://" + l.Addr().String()
	l.Close()

	// timeout: a daemon that accepts and never answers.
	release := make(chan struct{})
	hang := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	hang.StartTLS()
	t.Cleanup(func() { close(release); hang.Close() })
	hangPool := x509.NewCertPool()
	hangPool.AddCert(hang.Certificate())

	cases := []struct {
		name   string
		url    string
		pool   *x509.CertPool
		status int
		code   api.ErrorCode
	}{
		{"refused", refusedURL, nil, http.StatusBadGateway, api.ErrorCodeHostUnreachable},
		{"timeout", hang.URL, hangPool, http.StatusGatewayTimeout, api.ErrorCodeHostTimeout},
	}
	for _, c := range cases {
		ws := hostDownWorkstation(t, c.url, c.pool)
		for _, id := range names {
			body, fwd := forwardBodies[id]
			if !fwd {
				continue
			}
			if c.name == "timeout" && id == "waitVMIP" {
				// its client timeout is the wait plus a 30 s margin: too long for a test
				continue
			}
			t.Run(c.name+"/"+id, func(t *testing.T) {
				op := ops[id]
				if q, ok := forwardQuery[id]; ok {
					op[1] += q
				}
				var r *http.Request
				if body == "" {
					r = httptest.NewRequest(op[0], op[1], nil)
				} else {
					r = httptest.NewRequest(op[0], op[1], strings.NewReader(body))
					r.Header.Set("Content-Type", "application/json")
				}
				r.Header.Set("Authorization", "Bearer "+testToken)
				rec := httptest.NewRecorder()
				ws.ServeHTTP(rec, r)
				var e api.Error
				_ = json.Unmarshal(rec.Body.Bytes(), &e)
				if rec.Code != c.status || e.Code == nil || *e.Code != c.code {
					t.Errorf("%s %s: %d %s, want %d with code %s", op[0], op[1], rec.Code, rec.Body, c.status, c.code)
				}
			})
		}
	}
}

// forwardQuery is the query string an operation needs to get past validation.
var forwardQuery = map[string]string{"getUnderlay": "?to=192.0.2.10"}
