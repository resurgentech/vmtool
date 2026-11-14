package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const deployTok = "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"

// newDeployEnv builds on the cert environment (real openssl, stub bao/scp/ssh)
// and adds recording stubs for ssh, vmtool, git, tar, curl, sudo and
// ansible-playbook. The ssh stub answers `go env GOVERSION` from the files
// goanswer / goexit in the stub dir and records every other call's stdin in
// ssh.stdin, each preceded by a "--- <argv>" line.
func newDeployEnv(t *testing.T, goAnswer string) *certEnv {
	t.Helper()
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	if err := os.WriteFile(filepath.Join(e.stub, "goanswer"), []byte(goAnswer), 0o644); err != nil {
		t.Fatal(err)
	}
	addStub(t, e.stub, "ssh", `case "$*" in
  *"go env GOVERSION"*)
    cat "$d/goanswer"
    if [[ -f $d/goexit ]]; then exit "$(<"$d/goexit")"; fi
    exit 0 ;;
esac
{ echo "--- $*"; cat; } >> "$d/ssh.stdin"`)
	addStub(t, e.stub, "vmtool", `if [[ "$1 $2" == "config host" ]]; then echo "10.0.0.5 jared"; fi`)
	addStub(t, e.stub, "git", `printf 'cmd/vmtool/main.go\0go.mod\0'`)
	addStub(t, e.stub, "tar", `cat > "$d/tar.stdin"`)
	addStub(t, e.stub, "curl", `cat > "$d/curl.stdin"; echo '{"version":"x"}'`)
	addStub(t, e.stub, "sudo", `exit 1`)
	addStub(t, e.stub, "ansible-playbook", `while [[ $# -gt 0 ]]; do
  if [[ $1 == -i ]]; then cp "$2" "$d/inventory.copy"; fi
  shift
done`)
	if err := os.WriteFile(filepath.Join(e.wd, "go.mod"), []byte("module x\n\ngo 1.24.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.env = append(e.env, "VMTOOL_TOKEN="+deployTok, "DEPLOY_RETRIES=1", "GO_MIN=1.21")
	return e
}

func (e *certEnv) withoutToken() {
	var env []string
	for _, v := range e.env {
		if !strings.HasPrefix(v, "VMTOOL_TOKEN=") {
			env = append(env, v)
		}
	}
	e.env = env
}

func (e *certEnv) deploy(t *testing.T, extra []string, args ...string) result {
	return runScript(t, e.stub, e.wd, "deploy.sh", append(append([]string{}, e.env...), extra...), args...)
}

func TestDeployGoCheckFails(t *testing.T) {
	cases := []struct{ name, ans, exit, found string }{
		{"old", "go1.18.1\n", "", "go1.18.1"},
		{"numeric", "go1.9\n", "", "go1.9"},
		{"missing empty", "", "", "not installed"},
		{"missing 127", "", "127", "not installed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newDeployEnv(t, c.ans)
			if c.exit != "" {
				os.WriteFile(filepath.Join(e.stub, "goexit"), []byte(c.exit), 0o644)
			}
			r := e.deploy(t, nil, "veritech01")
			if r.code == 0 {
				t.Fatalf("exit 0: %s%s", r.stdout, r.stderr)
			}
			want := "veritech01: found " + c.found + ", need go >= 1.21 (go.mod asks for 1.24.0; GOTOOLCHAIN=auto fetches it)."
			if !strings.Contains(r.stderr, want) {
				t.Errorf("stderr lacks %q: %q", want, r.stderr)
			}
			for _, f := range []string{"tar.argv", "scp.argv", "git.argv", "bao.argv", "curl.argv"} {
				if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
					t.Errorf("%s recorded after a failed go check", f)
				}
			}
			ssh := readOr(t, filepath.Join(e.stub, "ssh.argv"))
			if strings.Count(ssh, "\n") != 1 || strings.Contains(ssh, "go build") || strings.Contains(ssh, "mkdir") {
				t.Errorf("ssh called beyond the go check: %q", ssh)
			}
		})
	}
}

func TestDeployGoCheckSshFailure(t *testing.T) {
	e := newDeployEnv(t, "")
	os.WriteFile(filepath.Join(e.stub, "goexit"), []byte("255"), 0o644)
	r := e.deploy(t, nil, "veritech01")
	if r.code == 0 || strings.Contains(r.stderr, "not installed") || !strings.Contains(r.stderr, "ssh") {
		t.Errorf("want an ssh failure message, got %d %q", r.code, r.stderr)
	}
}

func TestDeployGoCheckPasses(t *testing.T) {
	for _, v := range []string{"go1.21.0", "go1.22.2", "go1.24rc1", "go1.21", "go1.100.1"} {
		t.Run(v, func(t *testing.T) {
			e := newDeployEnv(t, v+"\n")
			r := e.deploy(t, nil, "veritech01")
			if r.code != 0 {
				t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
			}
		})
	}
}

func TestDeployFlow(t *testing.T) {
	e := newDeployEnv(t, "go1.22.2\n")
	r := e.deploy(t, nil, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	// The token is in no argv and not in output.
	for _, f := range []string{"ssh", "scp", "tar", "git", "curl", "vmtool", "sudo", "bao"} {
		if strings.Contains(readOr(t, filepath.Join(e.stub, f+".argv")), deployTok) {
			t.Errorf("token in %s argv", f)
		}
	}
	if strings.Contains(r.stdout+r.stderr, deployTok) {
		t.Error("token printed")
	}
	lines := strings.Split(strings.TrimSpace(readOr(t, filepath.Join(e.stub, "ssh.argv"))), "\n")
	find := func(sub string) int {
		for i, l := range lines {
			if strings.Contains(l, sub) {
				return i
			}
		}
		t.Errorf("no ssh call containing %q in %q", sub, lines)
		return -1
	}
	order := []int{
		find("go env GOVERSION"),
		find("tar -xzf - -C /home/jared/vmtool"),
		find("GOTOOLCHAIN=auto go build -o vmtool ./cmd/vmtool"),
		find("mkdir -p /home/jared/vmtool/tls"),
		find("config write --token-stdin --role hypervisor --name veritech01 --listen 0.0.0.0:9473 --tls-cert tls/veritech01.labtwo.crt --tls-key tls/veritech01.labtwo.key"),
		find("-t jared@10.0.0.5"),
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Errorf("steps out of order: %v", order)
		}
	}
	if l := lines[order[5]]; !strings.Contains(l, "sudo install -m 0644") || !strings.Contains(l, "sudo systemctl daemon-reload") ||
		!strings.Contains(l, "sudo systemctl enable --now vmtool") || !strings.Contains(l, "sudo systemctl restart vmtool") {
		t.Errorf("sudo call: %q", l)
	}
	if l := lines[order[4]]; strings.Contains(l, " -t ") {
		t.Errorf("token call must not use a tty: %q", l)
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "scp.argv")), "jared@10.0.0.5:/home/jared/vmtool/tls/") {
		t.Errorf("cert not copied: %q", readOr(t, filepath.Join(e.stub, "scp.argv")))
	}
	stdin := readOr(t, filepath.Join(e.stub, "ssh.stdin"))
	if !strings.Contains(stdin, "config write --token-stdin") || !strings.Contains(stdin, deployTok+"\n") {
		t.Errorf("token not sent on stdin of the config write call: %q", stdin)
	}
	if strings.Count(stdin, deployTok) != 1 {
		t.Errorf("token sent more than once: %d", strings.Count(stdin, deployTok))
	}
	for _, w := range []string{"User=jared", "WorkingDirectory=/home/jared/vmtool", "ExecStart=/home/jared/vmtool/vmtool server"} {
		if !strings.Contains(stdin, w) {
			t.Errorf("unit lacks %q", w)
		}
	}
	if strings.Contains(stdin, "@USER@") || strings.Contains(stdin, "@DIR@") {
		t.Error("unit placeholders not rendered")
	}
	// Only tracked files are archived.
	if got := readOr(t, filepath.Join(e.stub, "tar.stdin")); got != "cmd/vmtool/main.go\x00go.mod\x00" {
		t.Errorf("tar input %q", got)
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "git.argv")), "ls-files -z") {
		t.Error("files not taken from git ls-files")
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "tar.argv")), "--null") {
		t.Error("tar not given --null")
	}
	// GET /version without the token in argv, header on stdin, no -k.
	curl := readOr(t, filepath.Join(e.stub, "curl.argv"))
	for _, w := range []string{"--fail", "-H @-", "https://veritech01.labtwo:9473/version", "--resolve veritech01.labtwo:9473:10.0.0.5"} {
		if !strings.Contains(curl, w) {
			t.Errorf("curl argv lacks %q: %q", w, curl)
		}
	}
	if strings.Contains(curl, "-k") || strings.Contains(curl, "--insecure") {
		t.Errorf("curl skips verification: %q", curl)
	}
	if got := readOr(t, filepath.Join(e.stub, "curl.stdin")); got != "Authorization: Bearer "+deployTok+"\n" {
		t.Errorf("curl stdin %q", got)
	}
	if !strings.Contains(r.stdout, `{"version":"x"}`) {
		t.Errorf("version not printed: %q", r.stdout)
	}
	if _, err := os.Stat(filepath.Join(e.wd, "tls", "veritech01.labtwo.key")); err == nil {
		t.Error("local key kept")
	}
}

func TestDeployTokenFromDotEnvAndRemoteDir(t *testing.T) {
	e := newDeployEnv(t, "go1.22.2\n")
	e.withoutToken()
	os.WriteFile(filepath.Join(e.wd, ".env"), []byte("A=b\nVMTOOL_TOKEN="+deployTok+"\n"), 0o600)
	r := e.deploy(t, []string{"REMOTE_DIR=/srv/vmtool"}, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "ssh.argv")), "tar -xzf - -C /srv/vmtool") {
		t.Error("REMOTE_DIR ignored")
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "ssh.stdin")), "WorkingDirectory=/srv/vmtool") {
		t.Error("unit ignores REMOTE_DIR")
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "ssh.argv")), deployTok) {
		t.Error("token in argv")
	}
}

// A config.yml address that is a DNS name (veritech01.labtwo) must not be
// passed to curl --resolve, which accepts only numeric addresses.
func TestDeployHostnameAddressNoResolve(t *testing.T) {
	e := newDeployEnv(t, "go1.22.2\n")
	addStub(t, e.stub, "vmtool", `if [[ "$1 $2" == "config host" ]]; then echo "veritech01.labtwo jared"; fi`)
	r := e.deploy(t, nil, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	curl := readOr(t, filepath.Join(e.stub, "curl.argv"))
	if strings.Contains(curl, "--resolve") {
		t.Errorf("curl got --resolve for a DNS address: %q", curl)
	}
	if !strings.Contains(curl, "https://veritech01.labtwo:9473/version") {
		t.Errorf("curl URL: %q", curl)
	}
}

func TestDeployNoToken(t *testing.T) {
	e := newDeployEnv(t, "go1.22.2\n")
	e.withoutToken()
	r := e.deploy(t, nil, "veritech01")
	if r.code == 0 || !strings.Contains(r.stderr, "VMTOOL_TOKEN") {
		t.Errorf("want token error, got %d %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.stub, "ssh.argv")); err == nil {
		t.Error("ssh called without a token")
	}
}

func TestPrereqsHost(t *testing.T) {
	e := newDeployEnv(t, "")
	r := runScript(t, e.stub, e.wd, "prereqs.sh", e.env, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv := readOr(t, filepath.Join(e.stub, "ansible-playbook.argv"))
	for _, w := range []string{"setup_host.yml", "-e vmtool_hosts=veritech01", "-e vmtool_install_go=true", "-e vmtool_daemon_port=9473", "--ask-become-pass", "-i "} {
		if !strings.Contains(argv, w) {
			t.Errorf("argv lacks %q: %q", w, argv)
		}
	}
	inv := readOr(t, filepath.Join(e.stub, "inventory.copy"))
	if !strings.Contains(inv, "veritech01 ansible_host=10.0.0.5 ansible_user=jared") {
		t.Errorf("inventory: %q", inv)
	}
	if strings.Contains(argv, deployTok) {
		t.Error("token in argv")
	}
}

func TestPrereqsLocalUnchanged(t *testing.T) {
	e := newDeployEnv(t, "")
	r := runScript(t, e.stub, e.wd, "prereqs.sh", e.env, "-e", "vmtool_install_go=true")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv := readOr(t, filepath.Join(e.stub, "ansible-playbook.argv"))
	if strings.Contains(argv, "-i ") || strings.Contains(argv, "vmtool_hosts") || strings.Contains(argv, "daemon_port") {
		t.Errorf("local run changed: %q", argv)
	}
	if !strings.Contains(argv, "-e vmtool_install_go=true") {
		t.Errorf("args dropped: %q", argv)
	}
	if _, err := os.Stat(filepath.Join(e.stub, "vmtool.argv")); err == nil {
		t.Error("vmtool called for a local run")
	}
}

func TestSetupHostDaemonPort(t *testing.T) {
	b, err := os.ReadFile("../ansible/playbooks/setup_host.yml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, w := range []string{`vmtool_daemon_port: ""`, "ufw allow {{ vmtool_daemon_port }}/tcp"} {
		if !strings.Contains(s, w) {
			t.Errorf("setup_host.yml lacks %q", w)
		}
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook not installed")
	}
	cmd := exec.Command("ansible-playbook", "--syntax-check", "-i", "localhost,", "../ansible/playbooks/setup_host.yml")
	cmd.Env = append(os.Environ(), "ANSIBLE_LOCAL_TEMP="+t.TempDir(), "ANSIBLE_HOME="+t.TempDir())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("syntax-check: %v\n%s", err, out)
	}
}
