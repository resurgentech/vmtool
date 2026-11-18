package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/testexec"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const srvStdin = "KEY=PLACEHOLDER-not-a-secret-1\nit's=PLACEHOLDER-not-a-secret-2\n"

// ipMgr is a fakeMgr whose VM has an address.
type ipMgr struct{ *fakeMgr }

func (f ipMgr) Info(n string) (*vmtool.VMInfo, error) {
	return &vmtool.VMInfo{Name: n, IP: "10.0.0.5"}, nil
}

// execServer is a workstation handler chain with host "box" and machine
// nuc1, and a stub tunnel path.
func execServer(t *testing.T) http.Handler {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Kind: vmtool.MachineMetal, Host: "10.0.0.42"}); err != nil {
		t.Fatal(err)
	}
	res := hosts.NewWith("box", config.RoleWorkstation, nil, func() (app.Manager, func(), error) { return ipMgr{&fakeMgr{}}, func() {}, nil })
	return handlerFor(&handlers{hosts: res, role: config.RoleWorkstation, svc: &app.Service{}, tunnelExe: stubTunnelExe})
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

var execPaths = []string{"/v1/hosts/box/vms/web/exec", "/v1/metal/nuc1/exec"}

func execBody(t *testing.T, stdin *string) string {
	t.Helper()
	m := map[string]any{"command": []string{"cat"}}
	if stdin != nil {
		m["stdin"] = *stdin
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestExecStdinEchoed(t *testing.T) {
	for _, p := range execPaths {
		t.Run(p, func(t *testing.T) {
			h := execServer(t)
			testexec.Stub(t, map[string]string{"ssh": "#!/bin/sh\ncat\n", "sshpass": "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n"})
			in := srvStdin
			rec := do(h, "POST", p, execBody(t, &in))
			if rec.Code != 200 {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			var r struct {
				ExitCode int    `json:"exit_code"`
				Stdout   string `json:"stdout"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || r.Stdout != srvStdin || r.ExitCode != 0 {
				t.Fatalf("stdout %q exit %d err %v", r.Stdout, r.ExitCode, err)
			}
			// Absent stdin: empty standard input, as before.
			rec = do(h, "POST", p, execBody(t, nil))
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"stdout":""`) {
				t.Fatalf("absent: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestExecStdinNeverLeaks(t *testing.T) {
	stubs := map[string]map[string]string{
		"exit255":  {"ssh": "#!/bin/sh\necho 'ssh: connection failed' >&2\nexit 255\n", "sshpass": "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n"},
		"startErr": {"true": "#!/bin/sh\nexit 0\n"}, // no ssh, no sshpass: starting them fails
	}
	for name, st := range stubs {
		for _, p := range execPaths {
			t.Run(name+p, func(t *testing.T) {
				h := execServer(t)
				testexec.Stub(t, st)
				logs := captureLog(t)
				in := srvStdin
				rec := do(h, "POST", p, execBody(t, &in))
				// Both cases must take their failure path, or the leak check
				// proves nothing: a start error is a 5xx; ssh's own failure
				// (255) is a 200 carrying exit_code 255, as before.
				if name == "startErr" && rec.Code < 500 {
					t.Fatalf("start error answered %d %s", rec.Code, rec.Body)
				}
				if name == "exit255" && (rec.Code != 200 || !strings.Contains(rec.Body.String(), `"exit_code":255`)) {
					t.Fatalf("exit 255 answered %d %s", rec.Code, rec.Body)
				}
				for _, s := range []string{rec.Body.String(), logs.String()} {
					if strings.Contains(s, "PLACEHOLDER") {
						t.Fatalf("stdin text leaked:\n%s", s)
					}
				}
			})
		}
	}
}
