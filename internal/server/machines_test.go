package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

func TestMachineRoutes(t *testing.T) {
	t.Chdir(t.TempDir())
	ts := httptest.NewServer(handlerFor(&handlers{svc: &app.Service{PlaybookDir: "ansible/playbooks"}}))
	t.Cleanup(ts.Close)

	post := func(path string, body any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		res, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	get := func(path string) *http.Response {
		t.Helper()
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}

	res := get("/v1/metal")
	var list []map[string]any
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil || res.StatusCode != 200 || len(list) != 0 {
		t.Fatalf("empty list: %d %v %v", res.StatusCode, list, err)
	}

	for _, tc := range []struct {
		name string
		res  *http.Response
		want int
	}{
		{"get missing", get("/v1/metal/nuc1"), 404},
		{"exec missing", post("/v1/metal/nuc1/exec", map[string]any{"command": []string{"true"}}), 404},
		{"reboot missing", post("/v1/metal/nuc1/reboot", nil), 404},
		{"playbook commission", post("/v1/metal/nuc1/playbook", map[string]any{"playbook": "commission.yml"}), 400},
		{"add commission metal", post("/v1/metal", map[string]any{"name": "nuc1", "host": "10.0.0.42", "commission": true}), 400},
		{"add bad kind", post("/v1/metal", map[string]any{"name": "nuc1", "host": "10.0.0.42", "kind": "vm"}), 400},
	} {
		if tc.res.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, tc.res.StatusCode, tc.want)
		}
	}

	// DELETE forgets a registered machine.
	if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Kind: vmtool.MachineMetal, Host: "192.0.2.1"}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/metal/nuc1", nil)
	dres, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dres.Body.Close()
	if dres.StatusCode != 204 || vmtool.IsMachine("nuc1") {
		t.Fatalf("delete: %d, still registered=%v", dres.StatusCode, vmtool.IsMachine("nuc1"))
	}
}
