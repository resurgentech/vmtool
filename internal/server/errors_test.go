package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/testexec"
)

func errCode(t *testing.T, rec *httptest.ResponseRecorder) api.ErrorCode {
	t.Helper()
	var e api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code == nil {
		t.Fatalf("%d %s: no code (%v)", rec.Code, rec.Body, err)
	}
	return *e.Code
}

// Every app error kind has one code, and a kind added later gets `internal`
// until it is given one.
func TestCodeForKind(t *testing.T) {
	for k, want := range map[app.Kind]api.ErrorCode{
		app.KindInvalid:         api.ErrorCodeBadRequest,
		app.KindNotFound:        api.ErrorCodeNotFound,
		app.KindConflict:        api.ErrorCodeConflict,
		app.KindBadGateway:      api.ErrorCodeBadGateway,
		app.KindInternal:        api.ErrorCodeInternal,
		app.KindUnavailable:     api.ErrorCodeStoreUnavailable,
		app.KindHostUnreachable: api.ErrorCodeHostUnreachable,
		app.KindHostTimeout:     api.ErrorCodeHostTimeout,
	} {
		if got := codeForKind(k); got != want {
			t.Errorf("codeForKind(%d) = %s, want %s", k, got, want)
		}
	}
}

// The 401 body gains the code; its text is unchanged.
func TestUnauthorizedBodyCode(t *testing.T) {
	rec := httptest.NewRecorder()
	authedServer().Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/hosts", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
	var e api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error != "unauthorized: missing or invalid bearer token" || e.Code == nil || *e.Code != api.ErrorCodeUnauthorized {
		t.Errorf("body %s", rec.Body)
	}
}

// A refusal answered with its own status carries the matching code.
func TestRefusalCodes(t *testing.T) {
	h := execServer(t)
	if rec := do(h, "GET", "/v1/hosts/nowhere/vms", ""); rec.Code != 404 || errCode(t, rec) != api.ErrorCodeNotFound {
		t.Errorf("unknown host: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/v1/hosts/box/vms/web/exec", `{"command":[]}`); rec.Code != 400 || errCode(t, rec) != api.ErrorCodeBadRequest {
		t.Errorf("empty command: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/v1/hosts/box/images/web.qcow2/fetch", `{}`); rec.Code != 403 || errCode(t, rec) != api.ErrorCodeBadRequest {
		t.Errorf("a hypervisor-only operation on a workstation: %d %s", rec.Code, rec.Body)
	}
}

// execVM's ssh to the guest failing stays 502, as bad_gateway: the guest, not
// a host, did not answer.
func TestExecVMSSHFailureIsBadGateway(t *testing.T) {
	h := execServer(t)
	testexec.Stub(t, map[string]string{}) // no ssh to start
	rec := do(h, "POST", "/v1/hosts/box/vms/web/exec", `{"command":["true"]}`)
	if rec.Code != http.StatusBadGateway || errCode(t, rec) != api.ErrorCodeBadGateway {
		t.Errorf("%d %s, want 502 bad_gateway", rec.Code, rec.Body)
	}
}

// The hypervisor's refusal of a workstation-only operation says so.
func TestWorkstationOnlyCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeWorkstationOnly(rec, "createVM", "veritech01")
	if rec.Code != 403 || errCode(t, rec) != api.ErrorCodeWorkstationOnly {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}
