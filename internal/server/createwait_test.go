package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/internal/testexec"
)

// stubTunnelExe is a tunnel path that is not a test binary; the stub ansible
// never runs it.
const stubTunnelExe = "/opt/vmtool/vmtool"

// noIPMgr is a fakeMgr whose address wait fails like a timeout.
type noIPMgr struct{ *fakeMgr }

func (f noIPMgr) WaitForIP(n string, d time.Duration) (string, error) {
	f.rec("WaitForIP %s", n)
	f.waited = d
	return "", errors.New("timed out waiting for IP")
}

// createServer is a workstation handler chain for host "box" backed by m,
// with stub ansible and a stub tunnel path (create waits for SSH).
func createServer(t *testing.T, m app.Manager, svc *app.Service) http.Handler {
	t.Helper()
	t.Chdir(t.TempDir())
	testexec.Ansible(t)
	res := hosts.NewWith("box", config.RoleWorkstation, nil, func() (app.Manager, func(), error) { return m, func() {}, nil })
	return handlerFor(&handlers{hosts: res, role: config.RoleWorkstation, svc: svc, tunnelExe: stubTunnelExe})
}

func TestCreateVMNoAddressIs500WithVM(t *testing.T) {
	fm := &fakeMgr{}
	h := createServer(t, noIPMgr{fm}, &app.Service{IPTimeout: 9 * time.Second})
	rec := do(h, "POST", "/v1/hosts/box/vms", `{"name":"web","image":"u.qcow2"}`)
	if rec.Code != 500 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var e struct {
		Error string          `json:"error"`
		VM    json.RawMessage `json:"vm"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if len(e.VM) == 0 || !strings.Contains(string(e.VM), `"web"`) {
		t.Errorf("no vm in the error body: %s", rec.Body)
	}
	if !strings.Contains(e.Error, "got no address within 9s") || !strings.Contains(e.Error, "--no-wait") {
		t.Errorf("error = %q", e.Error)
	}
	if got := strings.Join(fm.calls, ","); strings.Contains(got, "Destroy") || strings.Contains(got, "Delete") {
		t.Errorf("the VM was removed: %s", got)
	}
}

func TestCreateVMNoWait(t *testing.T) {
	fm := &fakeMgr{}
	h := createServer(t, noIPMgr{fm}, &app.Service{})
	rec := do(h, "POST", "/v1/hosts/box/vms", `{"name":"web","image":"u.qcow2","no_wait":true}`)
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(fm.calls, ","); strings.Contains(got, "WaitForIP") || !strings.Contains(got, "Start web") {
		t.Errorf("calls: %s", got)
	}
}

func TestCreateVMNoWaitWithPlaybookIs400(t *testing.T) {
	fm := &fakeMgr{}
	h := createServer(t, fm, &app.Service{})
	rec := do(h, "POST", "/v1/hosts/box/vms", `{"name":"web","image":"u.qcow2","no_wait":true,"playbook":"x.yml"}`)
	if rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no-wait") || !strings.Contains(rec.Body.String(), "playbook") {
		t.Errorf("message names neither option: %s", rec.Body)
	}
	if len(fm.calls) != 0 {
		t.Errorf("calls before refusing: %v", fm.calls)
	}
}

func TestCreateVMWaitSSHFailureIs500WithVM(t *testing.T) {
	t.Setenv("STUB_WAIT_FAIL", "1")
	fm := &fakeMgr{ip: "10.0.0.5"}
	now := time.Unix(1000, 0)
	h := createServer(t, fm, &app.Service{
		Now:      func() time.Time { return now },
		SSHSleep: func(d time.Duration) { now = now.Add(d) },
	})
	rec := do(h, "POST", "/v1/hosts/box/vms", `{"name":"web","image":"u.qcow2"}`)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "did not accept SSH") || !strings.Contains(rec.Body.String(), `"vm"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestServerServiceGetsConfigWaits(t *testing.T) {
	s := &Server{Config: &config.Config{IPWaitSeconds: 45, SSHWaitSeconds: 50}}
	s.Handler()
	if got := s.h.svc; got.IPTimeout != 45*time.Second || got.SSHTimeout != 50*time.Second {
		t.Errorf("IPTimeout %s SSHTimeout %s", got.IPTimeout, got.SSHTimeout)
	}
}
