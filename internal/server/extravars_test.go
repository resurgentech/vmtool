package server

import (
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
)

func TestBadExtraVarNameIs400(t *testing.T) {
	h := createServer(t, &fakeMgr{ip: "10.0.0.5"}, &app.Service{PlaybookDir: "pb"})
	for _, tc := range []struct{ name, path, body string }{
		{"runPlaybook", "/v1/hosts/box/vms/web/playbook", `{"playbook":"x.yml","extra_vars":{"foo-bar":"PLACEHOLDER-not-a-secret-1"}}`},
		{"runMachinePlaybook", "/v1/metal/nuc1/playbook", `{"playbook":"x.yml","extra_vars":{"foo-bar":"PLACEHOLDER-not-a-secret-1"}}`},
		{"createVM", "/v1/hosts/box/vms", `{"name":"web","image":"u.qcow2","playbook":"x.yml","extra_vars":{"foo-bar":"PLACEHOLDER-not-a-secret-1"}}`},
	} {
		rec := do(h, "POST", tc.path, tc.body)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "PLACEHOLDER") {
			t.Errorf("%s: value in body: %s", tc.name, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "foo-bar") {
			t.Errorf("%s: body does not name the variable: %s", tc.name, rec.Body)
		}
	}
}
