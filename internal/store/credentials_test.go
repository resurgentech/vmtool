package store

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	phUUID1 = "00000000-0000-4000-8000-000000000001"
	phUUID2 = "00000000-0000-4000-8000-000000000002"
	phPass  = "PLACEHOLDER-not-a-secret-2"
	phKey   = "PLACEHOLDER-KEY-not-a-key"
	phPub   = "PLACEHOLDER-PUBKEY-not-a-key"
)

func vmBody(uuid string) string {
	b, _ := json.Marshal(map[string]string{"uuid": uuid, "username": "PLACEHOLDER-user", "password": phPass, "private_key": phKey, "public_key": phPub})
	return b2s(b)
}

func b2s(b []byte) string { return string(b) }

func (e *env) cred(t *testing.T, method, p, body string) (int, []byte) {
	t.Helper()
	if body == "" {
		resp, b := e.call(t, method, p, "Bearer "+tokA, nil, nil)
		return resp.StatusCode, b
	}
	resp, b := e.call(t, method, p, "Bearer "+tokA, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
	return resp.StatusCode, b
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %q", b)
	}
	return m
}

func TestVMCredentialRoundTrip(t *testing.T) {
	e := newEnv(t)
	vm := "/v1/credentials/vms/PLACEHOLDER-HOST/vm1"
	code, b := e.cred(t, "PUT", vm, vmBody(phUUID1))
	if code != 200 {
		t.Fatalf("put = %d %s", code, b)
	}
	m := decode(t, b)
	for _, f := range []string{"password", "private_key", "public_key", "username"} {
		if _, ok := m[f]; ok {
			t.Errorf("PUT response has %s", f)
		}
	}
	if m["uuid"] != phUUID1 || m["host"] != "PLACEHOLDER-HOST" || m["name"] != "vm1" || m["updated"] == nil {
		t.Errorf("put body %v", m)
	}
	code, b = e.cred(t, "GET", vm+"?uuid="+strings.ToUpper(phUUID1), "")
	if code != 200 {
		t.Fatalf("get = %d %s", code, b)
	}
	m = decode(t, b)
	want := map[string]string{"host": "PLACEHOLDER-HOST", "name": "vm1", "uuid": phUUID1, "username": "PLACEHOLDER-user", "password": phPass, "private_key": phKey, "public_key": phPub}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if m["updated"] == nil {
		t.Error("no updated")
	}

	// another uuid: stale, and not one credential field
	code, b = e.cred(t, "GET", vm+"?uuid="+phUUID2, "")
	if code != 404 {
		t.Fatalf("stale get = %d", code)
	}
	m = decode(t, b)
	if m["stale"] != true || len(m) != 2 {
		t.Errorf("stale body %v", m)
	}
	if strings.Contains(string(b), phPass) || strings.Contains(string(b), phKey) || strings.Contains(string(b), "PLACEHOLDER-user") {
		t.Error("stale body carries a credential")
	}

	// no record: 404 without stale
	code, b = e.cred(t, "GET", "/v1/credentials/vms/PLACEHOLDER-HOST/other?uuid="+phUUID1, "")
	if code != 404 {
		t.Fatalf("missing get = %d", code)
	}
	if m := decode(t, b); m["stale"] == true {
		t.Errorf("missing record is stale: %v", m)
	}

	// PUT with a new uuid replaces
	if code, b = e.cred(t, "PUT", vm, vmBody(phUUID2)); code != 200 {
		t.Fatalf("replace = %d %s", code, b)
	}
	if code, _ = e.cred(t, "GET", vm+"?uuid="+phUUID1, ""); code != 404 {
		t.Errorf("old uuid after replace = %d", code)
	}
	if code, _ = e.cred(t, "GET", vm+"?uuid="+phUUID2, ""); code != 200 {
		t.Errorf("new uuid after replace = %d", code)
	}

	if code, _ = e.cred(t, "DELETE", vm, ""); code != 204 {
		t.Errorf("delete = %d", code)
	}
	if code, b = e.cred(t, "GET", vm+"?uuid="+phUUID2, ""); code != 404 || decode(t, b)["stale"] == true {
		t.Errorf("get after delete = %d %s", code, b)
	}
	if code, _ = e.cred(t, "DELETE", vm, ""); code != 404 {
		t.Errorf("second delete = %d", code)
	}
}

func TestVMCredentialValidation(t *testing.T) {
	e := newEnv(t)
	ok := "/v1/credentials/vms/PLACEHOLDER-HOST/vm1"
	big := strings.Repeat("x", 64*1024+1)
	cases := []struct {
		name, method, path, body, ctype, field string
		code                                   int
	}{
		{"host with dot", "PUT", "/v1/credentials/vms/a.b/vm1", vmBody(phUUID1), "application/json", "host", 400},
		{"bad name", "PUT", "/v1/credentials/vms/hv1/-bad", vmBody(phUUID1), "application/json", "name", 400},
		{"bad uuid", "PUT", ok, vmBody("not-a-uuid"), "application/json", "uuid", 400},
		{"missing uuid", "PUT", ok, `{"username":"u"}`, "application/json", "uuid", 400},
		{"missing username", "PUT", ok, `{"uuid":"` + phUUID1 + `"}`, "application/json", "username", 400},
		{"oversize field", "PUT", ok, `{"uuid":"` + phUUID1 + `","username":"u","password":"` + big + `"}`, "application/json", "password", 400},
		{"NUL in key", "PUT", ok, `{"uuid":"` + phUUID1 + `","username":"u","private_key":"a\u0000b"}`, "application/json", "private_key", 400},
		{"malformed json", "PUT", ok, `{"uuid":"` + phUUID1 + `","password":"` + phPass + `"`, "application/json", "", 400},
		{"wrong content type", "PUT", ok, vmBody(phUUID1), "text/plain", "", 400},
		{"get missing uuid", "GET", ok, "", "", "uuid", 400},
		{"get bad uuid", "GET", ok + "?uuid=" + phPass, "", "", "uuid", 400},
		{"get bad host", "GET", "/v1/credentials/vms/a.b/vm1?uuid=" + phUUID1, "", "", "host", 400},
		{"delete bad name", "DELETE", "/v1/credentials/vms/hv1/-bad", "", "", "name", 400},
	}
	for _, c := range cases {
		hdr := map[string]string{}
		if c.ctype != "" {
			hdr["Content-Type"] = c.ctype
		}
		var rd *strings.Reader
		if c.body != "" {
			rd = strings.NewReader(c.body)
		}
		var resp int
		var b []byte
		if rd != nil {
			r, bb := e.call(t, c.method, c.path, "Bearer "+tokA, rd, hdr)
			resp, b = r.StatusCode, bb
		} else {
			r, bb := e.call(t, c.method, c.path, "Bearer "+tokA, nil, hdr)
			resp, b = r.StatusCode, bb
		}
		if resp != c.code {
			t.Errorf("%s = %d %s, want %d", c.name, resp, b, c.code)
			continue
		}
		s := string(b)
		for _, bad := range []string{phPass, phKey, "a.b", "not-a-uuid", "xxxxxxxx"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s: body echoes input: %s", c.name, s[:min(len(s), 200)])
			}
		}
		if c.field != "" && !strings.Contains(s, c.field) {
			t.Errorf("%s: body %q does not name %q", c.name, s, c.field)
		}
	}
	// nothing was stored by any of them
	if code, _ := e.cred(t, "GET", ok+"?uuid="+phUUID1, ""); code != 404 {
		t.Errorf("a rejected PUT stored a record: %d", code)
	}
}

func TestVMCredentialOversizeBody(t *testing.T) {
	e := newEnv(t)
	body := `{"uuid":"` + phUUID1 + `","username":"u","password":"` + strings.Repeat("y", 2<<20) + `"}`
	code, b := e.cred(t, "PUT", "/v1/credentials/vms/hv1/vm1", body)
	if code != 400 && code != 413 {
		t.Fatalf("oversize body = %d", code)
	}
	if strings.Contains(string(b), "yyyy") {
		t.Error("echoes the body")
	}
}

func TestMachineCredentials(t *testing.T) {
	e := newEnv(t)
	put := func(name, kind string) (int, []byte) {
		b, _ := json.Marshal(map[string]string{"kind": kind, "address": "PLACEHOLDER-ADDR", "username": "u", "password": phPass, "private_key": phKey, "public_key": phPub})
		return e.cred(t, "PUT", "/v1/credentials/machines/"+name, string(b))
	}
	if code, b := put("m2", "alien"); code != 200 || strings.Contains(string(b), phPass) || strings.Contains(string(b), "PLACEHOLDER-ADDR") {
		t.Fatalf("put = %d %s", code, b)
	}
	if code, b := put("m1", "metal"); code != 200 {
		t.Fatalf("put = %d %s", code, b)
	}
	code, b := e.cred(t, "GET", "/v1/credentials/machines/m1", "")
	if code != 200 {
		t.Fatalf("get = %d", code)
	}
	m := decode(t, b)
	for k, v := range map[string]string{"name": "m1", "kind": "metal", "address": "PLACEHOLDER-ADDR", "username": "u", "password": phPass, "private_key": phKey, "public_key": phPub} {
		if m[k] != v {
			t.Errorf("%s = %v", k, m[k])
		}
	}
	code, b = e.cred(t, "GET", "/v1/credentials/machines", "")
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	var list struct {
		Machines []map[string]any `json:"machines"`
	}
	if err := json.Unmarshal(b, &list); err != nil || len(list.Machines) != 2 {
		t.Fatalf("list %s", b)
	}
	if list.Machines[0]["name"] != "m1" || list.Machines[1]["name"] != "m2" || list.Machines[1]["kind"] != "alien" {
		t.Errorf("list %s", b)
	}
	for _, mm := range list.Machines {
		if len(mm) != 2 {
			t.Errorf("list entry has extra fields: %v", mm)
		}
	}
	for _, bad := range []string{phPass, phKey, phPub, "PLACEHOLDER-ADDR", "address", "password"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("list contains %s", bad)
		}
	}
	// replace changes kind
	put("m1", "alien")
	_, b = e.cred(t, "GET", "/v1/credentials/machines/m1", "")
	if decode(t, b)["kind"] != "alien" {
		t.Error("PUT did not replace")
	}
	if code, b := put("m3", "vm"); code != 400 || !strings.Contains(string(b), "kind") {
		t.Errorf("bad kind = %d %s", code, b)
	}
	if code, b := e.cred(t, "PUT", "/v1/credentials/machines/m3", `{"kind":"metal","address":"a\nb","username":"u"}`); code != 400 || !strings.Contains(string(b), "address") {
		t.Errorf("control char in address = %d %s", code, b)
	}
	if code, b := e.cred(t, "PUT", "/v1/credentials/machines/m3", `{"kind":"metal","address":"","username":"u"}`); code != 400 || !strings.Contains(string(b), "address") {
		t.Errorf("empty address = %d %s", code, b)
	}
	if code, b := put("-bad", "metal"); code != 400 || !strings.Contains(string(b), "name") {
		t.Errorf("bad name = %d %s", code, b)
	}
	if code, _ := e.cred(t, "DELETE", "/v1/credentials/machines/m1", ""); code != 204 {
		t.Errorf("delete = %d", code)
	}
	if code, _ := e.cred(t, "GET", "/v1/credentials/machines/m1", ""); code != 404 {
		t.Errorf("get after delete = %d", code)
	}
	if code, _ := e.cred(t, "DELETE", "/v1/credentials/machines/m1", ""); code != 404 {
		t.Errorf("second delete = %d", code)
	}
}

func TestCredentialRoutesNeedBearer(t *testing.T) {
	e := newEnv(t)
	tk := func() string {
		e.mustPush(t, "A", []byte("PLACEHOLDER-image"))
		return e.mint(t, "A").Ticket
	}()
	n := 0
	for op, public := range specOps(t) {
		if public || !strings.Contains(op, "/v1/credentials") {
			continue
		}
		method, p, _ := strings.Cut(op, " ")
		p = strings.NewReplacer("{name}", "m1", "{host}", "hv1").Replace(p)
		if strings.Contains(p, "/vms/") {
			p += "?uuid=" + phUUID1
		}
		for _, auth := range []string{"Ticket " + tk, "Bearer PLACEHOLDER-not-a-secret-3", ""} {
			resp, b := e.call(t, method, p, auth, strings.NewReader(vmBody(phUUID1)), nil)
			if resp.StatusCode != 401 {
				t.Errorf("%s with %q = %d %s", op, auth[:min(len(auth), 6)], resp.StatusCode, b)
			}
		}
		n++
	}
	if n != 7 {
		t.Errorf("checked %d credential operations, want 7", n)
	}
}

// The Must of the step: with slog at Debug, the secrets of a whole credential
// session appear in no log line and in no response except a successful GET.
func TestCredentialSecretsNeverLeak(t *testing.T) {
	e := newEnv(t)
	vm := "/v1/credentials/vms/PLACEHOLDER-HOST/vm1"
	machineBody := `{"kind":"metal","address":"PLACEHOLDER-ADDR","username":"u","password":"` + phPass + `","private_key":"` + phKey + `"}`

	// responses of successful GETs are the only place a secret may appear;
	// collect everything else through e.seen and take the GET bodies out of it.
	var okGets []string
	get := func(p string) {
		code, b := e.cred(t, "GET", p, "")
		if code == 200 {
			okGets = append(okGets, string(b))
		}
	}
	e.cred(t, "PUT", vm, vmBody(phUUID1))
	get(vm + "?uuid=" + phUUID1)
	e.cred(t, "PUT", vm, `{"uuid":"`+phUUID1+`","username":"u","password":"`+phPass+`","private_key":"`+phKey+`",`) // malformed
	e.cred(t, "PUT", vm, `{"uuid":"bad","username":"u","password":"`+phPass+`","private_key":"`+phKey+`"}`)         // invalid
	e.cred(t, "PUT", vm, `{"uuid":"`+phUUID1+`","username":"u","password":["`+phPass+`"]}`)                         // wrong type
	e.call(t, "PUT", vm, "Bearer "+tokA, strings.NewReader(vmBody(phUUID1)), map[string]string{"Content-Type": "text/plain"})
	e.call(t, "PUT", vm, "Bearer "+tokB+"x", strings.NewReader(vmBody(phUUID1)), map[string]string{"Content-Type": "application/json"})
	e.cred(t, "GET", vm+"?uuid="+phUUID2, "") // stale
	e.cred(t, "PUT", "/v1/credentials/machines/m1", machineBody)
	get("/v1/credentials/machines/m1")
	e.cred(t, "GET", "/v1/credentials/machines", "")
	e.cred(t, "PUT", "/v1/credentials/machines/m1", strings.Replace(machineBody, "metal", "bogus", 1))
	e.cred(t, "DELETE", vm, "")
	e.cred(t, "DELETE", "/v1/credentials/machines/m1", "")
	if len(okGets) != 2 {
		t.Fatalf("expected two successful GETs, got %d", len(okGets))
	}

	e.mu.Lock()
	seen := e.seen.String()
	e.mu.Unlock()
	for _, g := range okGets {
		// remove each successful GET body (once) from what was recorded
		if !strings.Contains(seen, g) {
			t.Fatal("test bug: GET body not recorded")
		}
		seen = strings.Replace(seen, g, "", 1)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "route=") {
		t.Fatalf("no request lines were captured: %q", logs)
	}
	for what, hay := range map[string]string{"log": logs, "responses": seen} {
		for _, secret := range []string{phPass, phKey, phPub, "PLACEHOLDER-user", "PLACEHOLDER-ADDR", tokA, tokB} {
			if strings.Contains(hay, secret) {
				t.Errorf("%s contains %q", what, secret)
			}
		}
	}
}

// The store never dumps or logs a request body. This scans the non-test
// sources of the store (and its command) for constructs that would: dumping a
// request, logging a Body, reading a whole request body into memory. The image
// push streams req.Body into the blob store and is not flagged.
func TestNoRequestBodyLogging(t *testing.T) {
	bad := regexp.MustCompile(`DumpRequest|DumpRequestOut|DumpResponse|ReadAll\(\s*(?:r|req|request)\.Body|(?:slog|log|s\.log|i\.log|h\.log|logger)\.\w+\([^)]*\b(?:r|req|request)\.Body|httputil\.NewSingleHostReverseProxy|"body",|"request_body"|"payload",`)
	roots := []string{".", "../../cmd/vmtool-store"}
	n := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			n++
			for i, line := range strings.Split(string(src), "\n") {
				if bad.MatchString(line) {
					t.Errorf("%s:%d logs or dumps a request body: %s", p, i+1, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n < 5 {
		t.Fatalf("scanned only %d files", n)
	}
	// the pattern must catch what it is for
	for _, s := range []string{`httputil.DumpRequest(r, true)`, `b, _ := io.ReadAll(r.Body)`, `s.log.Info("x", "b", r.Body)`} {
		if !bad.MatchString(s) {
			t.Errorf("pattern misses %q", s)
		}
	}
}
