package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type certEnv struct {
	stub, wd, caCrt, caKey string
	env                    []string
}

func sh(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

// newCertEnv makes a throwaway CA with real openssl (kept outside the work
// dir), a stub dir whose bao serves it, and a work dir holding config.yml.
func newCertEnv(t *testing.T, config string) *certEnv {
	t.Helper()
	ca := t.TempDir()
	sh(t, ca, "openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1",
		"-nodes", "-keyout", "ca.key", "-out", "ca.crt", "-subj", "/CN=test-ca", "-days", "30")
	e := &certEnv{stub: t.TempDir(), wd: t.TempDir(), caCrt: filepath.Join(ca, "ca.crt"), caKey: filepath.Join(ca, "ca.key")}
	addStub(t, e.stub, "bao", `if [[ "$1 $2 $3" == "kv get -field=string" ]]; then
  case "$4" in
    */crt) cat '`+e.caCrt+`' ;;
    */key) cat '`+e.caKey+`' ;;
    *) exit 3 ;;
  esac
fi`)
	addStub(t, e.stub, "scp", "exit 0")
	addStub(t, e.stub, "ssh", "exit 0")
	addStub(t, e.stub, "hostname", `echo fallbackhost`)
	if config != "" {
		if err := os.WriteFile(filepath.Join(e.wd, "config.yml"), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.env = []string{"VMTOOL_DOMAIN=labtwo", "BAO_CA_PATH=kv/infra/labtwo/selfsigned_cert", "CERT_DAYS=825",
		"VMTOOL_BIN=" + filepath.Join(e.stub, "vmtool")}
	return e
}

func (e *certEnv) run(t *testing.T, extra []string, args ...string) result {
	return runScript(t, e.stub, e.wd, "cert.sh", append(append([]string{}, e.env...), extra...), args...)
}

// noCAKey fails if the CA private key's PEM body appears in any file under
// the work and stub dirs.
func (e *certEnv) noCAKey(t *testing.T) {
	t.Helper()
	pem := readOr(t, e.caKey)
	var body string
	for _, l := range strings.Split(pem, "\n") {
		if l != "" && !strings.HasPrefix(l, "-----") {
			body = l
			break
		}
	}
	if len(body) < 20 {
		t.Fatalf("could not extract CA key body from %q", pem)
	}
	for _, d := range []string{e.wd, e.stub} {
		filepath.Walk(d, func(p string, fi os.FileInfo, err error) error {
			if err == nil && fi.Mode().IsRegular() {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), body) {
					t.Errorf("CA key material found in %s", p)
				}
			}
			return nil
		})
	}
}

func (e *certEnv) checkCert(t *testing.T, host string) {
	t.Helper()
	crt := filepath.Join(e.wd, "tls", host+".labtwo.crt")
	if out, err := exec.Command("openssl", "verify", "-CAfile", e.caCrt, crt).CombinedOutput(); err != nil {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	san := sh(t, e.wd, "openssl", "x509", "-noout", "-ext", "subjectAltName", "-in", crt)
	for _, want := range []string{"DNS:" + host + ".labtwo", "DNS:localhost", "IP Address:127.0.0.1"} {
		if !strings.Contains(san, want) {
			t.Errorf("SAN lacks %q:\n%s", want, san)
		}
	}
	keyPath := filepath.Join(e.wd, "tls", host+".labtwo.key")
	if _, err := os.Stat(keyPath); err == nil {
		a := sh(t, e.wd, "openssl", "x509", "-noout", "-pubkey", "-in", crt)
		if b := sh(t, e.wd, "openssl", "pkey", "-pubout", "-in", keyPath); a != b {
			t.Error("key does not match certificate")
		}
	}
}

func TestCertLocal(t *testing.T) {
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	r := e.run(t, nil, "jetfire")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	e.checkCert(t, "jetfire")
	fi, err := os.Stat(filepath.Join(e.wd, "tls", "jetfire.labtwo.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key: %v %v", fi, err)
	}
	for _, f := range []string{"scp.argv", "ssh.argv"} {
		if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
			t.Errorf("%s called for the local host", f)
		}
	}
	argv := readOr(t, filepath.Join(e.stub, "bao.argv"))
	for _, w := range []string{"kv get -field=string kv/infra/labtwo/selfsigned_cert/crt", "kv get -field=string kv/infra/labtwo/selfsigned_cert/key"} {
		if !strings.Contains(argv, w) {
			t.Errorf("bao argv lacks %q: %q", w, argv)
		}
	}
	ents, _ := os.ReadDir(e.wd)
	for _, en := range ents {
		if en.Name() != "tls" && en.Name() != "config.yml" {
			t.Errorf("stray file %s", en.Name())
		}
	}
	e.noCAKey(t)
}

func TestCertRemote(t *testing.T) {
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	addStub(t, e.stub, "vmtool", `if [[ "$1 $2 $3" == "config host optimus" ]]; then echo "10.0.0.5 alice"; else exit 1; fi`)
	addStub(t, e.stub, "scp", `for a; do [[ -f $a ]] && echo "$a $(stat -c %a "$a")" >> "$d/scp.files"; done; true`)
	r := e.run(t, nil, "optimus")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	e.checkCert(t, "optimus")
	if _, err := os.Stat(filepath.Join(e.wd, "tls", "optimus.labtwo.key")); err == nil {
		t.Error("local key not deleted after scp")
	}
	lines := strings.Split(strings.TrimSpace(readOr(t, filepath.Join(e.stub, "ssh.argv"))), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 ssh calls (stage dir, sudo install), got %q", lines)
	}
	if l := lines[0]; !strings.HasPrefix(l, "alice@10.0.0.5 ") || !strings.Contains(l, `umask 077 && mkdir -p "$HOME/.cache/vmtool-deploy/stage/tls"`) || strings.Contains(l, "sudo") {
		t.Errorf("staging call: %q", l)
	}
	scp := readOr(t, filepath.Join(e.stub, "scp.argv"))
	if !strings.Contains(scp, "optimus.labtwo.crt") || !strings.Contains(scp, "optimus.labtwo.key") ||
		!strings.Contains(scp, "alice@10.0.0.5:.cache/vmtool-deploy/stage/tls/") {
		t.Errorf("scp argv: %q", scp)
	}
	files := readOr(t, filepath.Join(e.stub, "scp.files"))
	if !regexp.MustCompile(`optimus\.labtwo\.key 600`).MatchString(files) {
		t.Errorf("key not 0600 when copied: %q", files)
	}
	inst := lines[1]
	if !strings.HasPrefix(inst, "-t alice@10.0.0.5 ") || strings.Count(inst, "sudo") != 1 {
		t.Errorf("want one ssh -t call with one sudo: %q", inst)
	}
	for _, w := range []string{
		"sudo sh -c",
		"install -o vmtool -g vmtool -m 0644 \"$1/tls/optimus.labtwo.crt\" /opt/vmtool/tls/optimus.labtwo.crt",
		"install -o vmtool -g vmtool -m 0600 \"$1/tls/optimus.labtwo.key\" /opt/vmtool/tls/optimus.labtwo.key",
		"for u in vmtool-daemon vmtool; do if systemctl cat $u >/dev/null 2>&1; then systemctl try-restart $u; fi; done",
		`rm -f "$s/tls/optimus.labtwo.key"`,
	} {
		if !strings.Contains(inst, w) {
			t.Errorf("install call lacks %q: %q", w, inst)
		}
	}
	if _, err := os.Stat(filepath.Join(e.stub, "sudo.argv")); err == nil {
		t.Error("sudo run on the workstation")
	}
	e.noCAKey(t)
}

// deploy sets CERT_STAGE_ONLY=1: cert.sh stages the files and runs no sudo.
func TestCertRemoteStageOnly(t *testing.T) {
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	addStub(t, e.stub, "vmtool", `echo "10.0.0.5 alice"`)
	r := e.run(t, []string{"CERT_STAGE_ONLY=1"}, "optimus")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	ssh := readOr(t, filepath.Join(e.stub, "ssh.argv"))
	if strings.Count(ssh, "\n") != 1 || strings.Contains(ssh, "sudo") || strings.Contains(ssh, "-t ") || strings.Contains(ssh, "try-restart") {
		t.Errorf("stage-only ran more than the staging call: %q", ssh)
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "scp.argv")), "alice@10.0.0.5:.cache/vmtool-deploy/stage/tls/") {
		t.Error("not staged")
	}
	if _, err := os.Stat(filepath.Join(e.wd, "tls", "optimus.labtwo.key")); err == nil {
		t.Error("local key not deleted after scp")
	}
}

// HV_DIR moves the install target; an old REMOTE_DIR is ignored.
func TestCertHVDirOverride(t *testing.T) {
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	addStub(t, e.stub, "vmtool", `echo "10.0.0.5 alice"`)
	r := e.run(t, []string{"HV_DIR=/srv/vm", "REMOTE_DIR=/elsewhere"}, "optimus")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	ssh := readOr(t, filepath.Join(e.stub, "ssh.argv"))
	if !strings.Contains(ssh, "/srv/vm/tls/optimus.labtwo.key") || strings.Contains(ssh, "/opt/vmtool") {
		t.Errorf("HV_DIR not used: %q", ssh)
	}
	if strings.Contains(ssh+readOr(t, filepath.Join(e.stub, "scp.argv")), "/elsewhere") {
		t.Error("REMOTE_DIR used")
	}
	if !strings.Contains(r.stderr, "REMOTE_DIR") {
		t.Errorf("no warning that REMOTE_DIR is ignored: %q", r.stderr)
	}
}

func TestCertUnsafeHVDir(t *testing.T) {
	for _, d := range []string{"srv/vm", "/srv/v m", "/srv/$(id)", "/srv/v'm", "/"} {
		t.Run(d, func(t *testing.T) {
			e := newCertEnv(t, "role: workstation\nname: jetfire\n")
			addStub(t, e.stub, "vmtool", `echo "10.0.0.5 alice"`)
			r := e.run(t, []string{"HV_DIR=" + d}, "optimus")
			if r.code == 0 || !strings.Contains(r.stderr, "HV_DIR") {
				t.Fatalf("want HV_DIR refusal, got %d %q", r.code, r.stderr)
			}
			for _, f := range []string{"bao.argv", "ssh.argv", "scp.argv"} {
				if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
					t.Errorf("%s called despite an unsafe HV_DIR", f)
				}
			}
		})
	}
}

// `just cert <host>` on its own, with the remote side run for real (see
// newRemoteEnv): staged, installed for vmtool, staged key removed,
// try-restart, one sudo.
func TestCertRemoteCommandsRun(t *testing.T) {
	e, rhome, hv := newRemoteEnv(t)
	r := e.run(t, []string{"HV_DIR=" + hv}, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	want := hv + "/tls/veritech01.labtwo.crt 0644\n" + hv + "/tls/veritech01.labtwo.key 0600\n"
	if got := readOr(t, filepath.Join(e.stub, "installed.list")); got != want {
		t.Errorf("installed:\n%s\nwant:\n%s", got, want)
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "installed", "veritech01.labtwo.key")), "PRIVATE KEY") {
		t.Error("installed key is not a key")
	}
	if got := strings.ReplaceAll(strings.TrimSpace(readOr(t, filepath.Join(e.stub, "systemctl.argv"))), "\n", ";"); got != "cat vmtool-daemon;try-restart vmtool-daemon;cat vmtool;try-restart vmtool" {
		t.Errorf("systemctl %q", got)
	}
	if n := strings.Count(readOr(t, filepath.Join(e.stub, "sudo.argv")), "\n"); n != 1 {
		t.Errorf("sudo ran %d times", n)
	}
	stage := filepath.Join(rhome, ".cache", "vmtool-deploy", "stage", "tls")
	if m := fileMode(t, stage); m != 0o700 {
		t.Errorf("stage/tls mode %v", m)
	}
	if _, err := os.Stat(filepath.Join(stage, "veritech01.labtwo.key")); err == nil {
		t.Error("staged key not removed")
	}
	if _, err := os.Stat(filepath.Join(e.wd, "tls", "veritech01.labtwo.key")); err == nil {
		t.Error("local key kept")
	}
	e.noCAKey(t)
}

// A host that still runs the older vmtool unit (no vmtool-daemon): only that
// unit is restarted, and the run does not fail on the missing one.
func TestCertRemoteOldUnitOnly(t *testing.T) {
	e, _, hv := newRemoteEnv(t)
	addStub(t, e.stub, "systemctl", `[[ "$1 $2" == "cat vmtool-daemon" ]] && exit 1; exit 0`)
	r := e.run(t, []string{"HV_DIR=" + hv}, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if got := strings.ReplaceAll(strings.TrimSpace(readOr(t, filepath.Join(e.stub, "systemctl.argv"))), "\n", ";"); got != "cat vmtool-daemon;cat vmtool;try-restart vmtool" {
		t.Errorf("systemctl %q", got)
	}
}

func TestCertScpFailureKeepsKey(t *testing.T) {
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	addStub(t, e.stub, "vmtool", `echo "10.0.0.5 alice"`)
	addStub(t, e.stub, "scp", "exit 1")
	r := e.run(t, nil, "optimus")
	if r.code == 0 {
		t.Fatal("exit 0 although scp failed")
	}
	if _, err := os.Stat(filepath.Join(e.wd, "tls", "optimus.labtwo.key")); err != nil {
		t.Error("key deleted although scp failed")
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "ssh.argv")), "sudo") {
		t.Error("install attempted although scp failed")
	}
}

func TestCertRefusals(t *testing.T) {
	cases := []struct {
		name, config string
		args         []string
		want         string
	}{
		{"hypervisor", "role: hypervisor\nname: jetfire\n", []string{"jetfire"}, "workstation"},
		{"no host", "role: workstation\n", nil, "usage"},
		{"bad host", "role: workstation\n", []string{"a/b"}, "host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newCertEnv(t, c.config)
			r := e.run(t, nil, c.args...)
			if r.code == 0 || !strings.Contains(r.stderr, c.want) {
				t.Fatalf("exit %d stderr %q, want failure mentioning %q", r.code, r.stderr, c.want)
			}
			if _, err := os.Stat(filepath.Join(e.stub, "bao.argv")); err == nil {
				t.Error("bao called despite refusal")
			}
			if _, err := os.Stat(filepath.Join(e.wd, "tls")); err == nil {
				t.Error("tls/ created despite refusal")
			}
		})
	}
}

// No config.yml: the default role is workstation, and the local name falls
// back to `hostname -s`. (install runs cert before it writes config.yml.)
func TestCertNoConfigIsWorkstation(t *testing.T) {
	e := newCertEnv(t, "")
	r := e.run(t, nil, "fallbackhost")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	e.checkCert(t, "fallbackhost")
	if _, err := os.Stat(filepath.Join(e.stub, "scp.argv")); err == nil {
		t.Error("scp called: fallbackhost should be the local host")
	}
}

func TestCertLocalName(t *testing.T) {
	e := newCertEnv(t, "name: \"jetfire\" # me\n")
	r := e.run(t, nil, "--local-name")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "jetfire" {
		t.Fatalf("exit %d out %q err %q", r.code, r.stdout, r.stderr)
	}
}

func TestInstall(t *testing.T) {
	e := newCertEnv(t, "")
	addStub(t, e.stub, "go", `if [[ $1 == build && $2 == -o ]]; then
  printf '#!/bin/sh\necho "$*" >> "$(dirname "$0")/vmtool.argv"\n' > "$3"; chmod +x "$3"
fi`)
	addStub(t, e.stub, "sudo", `exec "$@"`)
	addStub(t, e.stub, "systemctl", "exit 0")
	if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte("VMTOOL_TOKEN=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(t.TempDir(), "vmtool.service")
	env := append([]string{"UNIT_PATH=" + unit}, e.env[:3]...)
	r := runScript(t, e.stub, e.wd, "install.sh", env)
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if got := readOr(t, filepath.Join(e.stub, "go.argv")); !strings.Contains(got, "build -o "+e.wd+"/vmtool ./cmd/vmtool") {
		t.Errorf("go argv: %q", got)
	}
	e.checkCert(t, "fallbackhost")
	cfg := readOr(t, filepath.Join(e.wd, "vmtool.argv"))
	for _, w := range []string{"config write", "--role workstation", "--listen 127.0.0.1:9473",
		"--tls-cert tls/fallbackhost.labtwo.crt", "--tls-key tls/fallbackhost.labtwo.key"} {
		if !strings.Contains(cfg, w) {
			t.Errorf("config write argv lacks %q: %q", w, cfg)
		}
	}
	u := readOr(t, unit)
	me := strings.TrimSpace(sh(t, "", "id", "-un"))
	for _, w := range []string{"User=" + me, "WorkingDirectory=" + e.wd, "ExecStart=" + e.wd + "/vmtool server",
		"Restart=on-failure", "WantedBy=multi-user.target", "After=network-online.target libvirtd.service"} {
		if !strings.Contains(u, w) {
			t.Errorf("unit lacks %q:\n%s", w, u)
		}
	}
	if strings.Contains(u, "@") || strings.Contains(u, "abc") {
		t.Errorf("unit has unrendered placeholder or token:\n%s", u)
	}
	if fi, _ := os.Stat(unit); fi.Mode().Perm() != 0o644 {
		t.Errorf("unit mode %v", fi.Mode().Perm())
	}
	sudo := readOr(t, filepath.Join(e.stub, "sudo.argv"))
	if !strings.Contains(sudo, "install -m 0644") || !strings.Contains(sudo, unit) {
		t.Errorf("sudo argv: %q", sudo)
	}
	got := strings.ReplaceAll(strings.TrimSpace(readOr(t, filepath.Join(e.stub, "systemctl.argv"))), "\n", ";")
	if got != "daemon-reload;enable --now vmtool;restart vmtool" {
		t.Errorf("systemctl calls: %q", got)
	}
	if strings.Contains(r.stdout+r.stderr, "abc") {
		t.Error("token printed")
	}
	e.noCAKey(t)
	if _, err := exec.LookPath("systemd-analyze"); err != nil {
		t.Skip("systemd-analyze not installed")
	}
	if out, err := exec.Command("systemd-analyze", "verify", unit).CombinedOutput(); err != nil || len(out) > 0 {
		t.Errorf("systemd-analyze verify: %v\n%s", err, out)
	}
}

func TestInstallNeedsToken(t *testing.T) {
	e := newCertEnv(t, "")
	addStub(t, e.stub, "go", "exit 0")
	addStub(t, e.stub, "sudo", `exec "$@"`)
	r := runScript(t, e.stub, e.wd, "install.sh", append([]string{"UNIT_PATH=" + filepath.Join(e.wd, "u")}, e.env[:3]...))
	if r.code == 0 || !strings.Contains(r.stderr, "just token") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	for _, f := range []string{"go.argv", "sudo.argv", "bao.argv"} {
		if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
			t.Errorf("%s called before the token check", f)
		}
	}
}
