package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const deployTok = "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"

// sshRecord saves each ssh call's argv and stdin as ssh.<n>.argv and
// ssh.<n>.stdin (n counts from 1), after the generic stub header appended
// the argv to ssh.argv.
const sshRecord = `n=$(wc -l < "$d/ssh.argv"); n=${n//[^0-9]/}
printf '%s' "$*" > "$d/ssh.$n.argv"
cat > "$d/ssh.$n.stdin"
`

// newDeployEnv builds on the cert environment (real openssl, stub bao/scp/ssh)
// and adds recording stubs for ssh, vmtool, git, tar, curl, sudo and
// ansible-playbook. The ssh stub records every call (see sshRecord), answers
// `go env GOVERSION` from the files goanswer / goexit in the stub dir and the
// prereq check (`id vmtool ...`) with exit code prereqexit (default 0).
func newDeployEnv(t *testing.T, goAnswer string) *certEnv {
	t.Helper()
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	if err := os.WriteFile(filepath.Join(e.stub, "goanswer"), []byte(goAnswer), 0o644); err != nil {
		t.Fatal(err)
	}
	addStub(t, e.stub, "ssh", sshRecord+`case "$*" in
  *"go env GOVERSION"*)
    cat "$d/goanswer"
    if [[ -f $d/goexit ]]; then exit "$(<"$d/goexit")"; fi
    exit 0 ;;
  *"id vmtool"*)
    if [[ -f $d/prereqexit ]]; then exit "$(<"$d/prereqexit")"; fi
    exit 0 ;;
esac`)
	addStub(t, e.stub, "vmtool", `if [[ "$1 $2" == "config host" ]]; then echo "10.0.0.5 jared 9473"; fi`)
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

type sshCall struct {
	argv, stdin string
	tty         bool
}

// sshCalls returns the recorded ssh calls in order.
func sshCalls(t *testing.T, stub string) []sshCall {
	t.Helper()
	n := strings.Count(readOr(t, filepath.Join(stub, "ssh.argv")), "\n")
	var out []sshCall
	for i := 1; i <= n; i++ {
		a := readOr(t, filepath.Join(stub, "ssh."+strconv.Itoa(i)+".argv"))
		out = append(out, sshCall{argv: a, stdin: readOr(t, filepath.Join(stub, "ssh."+strconv.Itoa(i)+".stdin")),
			tty: strings.HasPrefix(a, "-t ")})
	}
	return out
}

// findCall returns the index of the first call whose argv contains sub.
func findCall(t *testing.T, calls []sshCall, sub string) int {
	t.Helper()
	for i, c := range calls {
		if strings.Contains(c.argv, sub) {
			return i
		}
	}
	t.Errorf("no ssh call containing %q", sub)
	return -1
}

// tokenNowhereBut fails if the token is in any stub-recorded file other than
// the stdin of ssh call `allowed` (1-based; 0 = none), or under extra dirs.
func tokenNowhereBut(t *testing.T, allowed int, dirs ...string) {
	t.Helper()
	keep := ""
	if allowed > 0 {
		keep = "ssh." + strconv.Itoa(allowed) + ".stdin"
	}
	for _, d := range dirs {
		filepath.Walk(d, func(p string, fi os.FileInfo, err error) error {
			if err != nil || !fi.Mode().IsRegular() || filepath.Base(p) == keep && filepath.Dir(p) == dirs[0] {
				return nil
			}
			if b, _ := os.ReadFile(p); strings.Contains(string(b), deployTok) {
				t.Errorf("token found in %s", p)
			}
			return nil
		})
	}
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

// A Go installed by `just prereqs <host>` in /usr/local/go must win over an
// older distro go on the login PATH: both the check and the build put it first.
func TestDeployGoPathFirst(t *testing.T) {
	e := newDeployEnv(t, "go1.22.2\n")
	if r := e.deploy(t, nil, "veritech01"); r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	calls := sshCalls(t, e.stub)
	for _, sub := range []string{"go env GOVERSION", "go build"} {
		i := findCall(t, calls, sub)
		if i < 0 {
			continue
		}
		if !strings.Contains(calls[i].argv, "PATH=/usr/local/go/bin:$PATH") || strings.Contains(calls[i].argv, "$PATH:/usr/local/go/bin") {
			t.Errorf("%s: /usr/local/go/bin not first on PATH: %q", sub, calls[i].argv)
		}
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
	calls := sshCalls(t, e.stub)
	const stage = `"$HOME/.cache/vmtool-deploy/stage`
	order := []int{
		findCall(t, calls, "go env GOVERSION"),
		findCall(t, calls, "id vmtool >/dev/null 2>&1 && test -d /opt/vmtool"),
		findCall(t, calls, `rm -rf "$HOME/.cache/vmtool-deploy/src" "$HOME/.cache/vmtool-deploy/stage"`),
		findCall(t, calls, "go build -o "+stage+`/vmtool" ./cmd/vmtool`),
		findCall(t, calls, `mkdir -p `+stage+`/tls"`),
		findCall(t, calls, "config write --token-stdin --role hypervisor --name veritech01 --listen 0.0.0.0:9473 --tls-cert tls/veritech01.labtwo.crt --tls-key tls/veritech01.labtwo.key"),
		findCall(t, calls, `cat > `+stage+`/vmtool-daemon.service"`),
		findCall(t, calls, "sudo sh -c"),
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			t.Fatalf("steps out of order: %v in %+v", order, calls)
		}
	}
	if len(calls) != len(order) {
		t.Errorf("want %d ssh calls, got %d: %+v", len(order), len(calls), calls)
	}
	copyCall, cfg, unit, inst := calls[order[2]], calls[order[5]], calls[order[6]], calls[order[7]]
	for _, w := range []string{"umask 077", `mkdir -p "$HOME/.cache/vmtool-deploy/src" ` + stage + `/tls"`, `tar -xzf - -C "$HOME/.cache/vmtool-deploy/src"`} {
		if !strings.Contains(copyCall.argv, w) {
			t.Errorf("copy call lacks %q: %q", w, copyCall.argv)
		}
	}
	// Token: exactly once, on stdin of the config write call, which has no
	// tty and no sudo; staged config.yml is written under umask 077 and
	// rebuilt from scratch.
	for i, c := range calls {
		n := strings.Count(c.stdin, deployTok)
		if i == order[5] {
			if n != 1 || c.stdin != deployTok+"\n" {
				t.Errorf("config write stdin %q", c.stdin)
			}
		} else if n != 0 {
			t.Errorf("token on stdin of ssh call %d: %q", i+1, c.argv)
		}
		if c.tty && n > 0 {
			t.Errorf("ssh -t call received the token: %q", c.argv)
		}
	}
	if cfg.tty || strings.Contains(cfg.argv, "sudo") {
		t.Errorf("config write call has a tty or sudo: %q", cfg.argv)
	}
	for _, w := range []string{`cd ` + stage + `"`, "umask 077", "rm -f config.yml", "./vmtool config write"} {
		if !strings.Contains(cfg.argv, w) {
			t.Errorf("config write call lacks %q: %q", w, cfg.argv)
		}
	}
	tokenNowhereBut(t, order[5]+1, e.stub, e.wd)
	// Exactly one tty call, one sudo, and it is the install.
	ttys, sudos := 0, 0
	for _, c := range calls {
		if c.tty {
			ttys++
		}
		sudos += strings.Count(c.argv, "sudo")
	}
	if ttys != 1 || sudos != 1 || !inst.tty {
		t.Errorf("want one ssh -t call with one sudo (the install): %d tty, %d sudo", ttys, sudos)
	}
	if _, err := os.Stat(filepath.Join(e.stub, "sudo.argv")); err == nil {
		t.Error("sudo run on the workstation")
	}
	for _, w := range []string{
		"install -o vmtool -g vmtool -m 0755 \"$1/vmtool\" /opt/vmtool/vmtool",
		"install -o vmtool -g vmtool -m 0600 \"$1/config.yml\" /opt/vmtool/config.yml",
		"install -o vmtool -g vmtool -m 0644 \"$1/tls/veritech01.labtwo.crt\" /opt/vmtool/tls/veritech01.labtwo.crt",
		"install -o vmtool -g vmtool -m 0600 \"$1/tls/veritech01.labtwo.key\" /opt/vmtool/tls/veritech01.labtwo.key",
		"install -m 0644 \"$1/vmtool-daemon.service\" /etc/systemd/system/vmtool-daemon.service",
		`rm -f "$s/config.yml" "$s/tls/veritech01.labtwo.key"`,
		"systemctl daemon-reload; systemctl enable --now vmtool-daemon; systemctl restart vmtool-daemon",
	} {
		if !strings.Contains(inst.argv, w) {
			t.Errorf("install call lacks %q: %q", w, inst.argv)
		}
	}
	// The unit, rendered for the vmtool user and /opt/vmtool.
	for _, w := range []string{"User=vmtool", "WorkingDirectory=/opt/vmtool", "ExecStart=/opt/vmtool/vmtool server"} {
		if !strings.Contains(unit.stdin, w) {
			t.Errorf("unit lacks %q: %q", w, unit.stdin)
		}
	}
	if strings.Contains(unit.stdin, "@USER@") || strings.Contains(unit.stdin, "@DIR@") || unit.tty {
		t.Errorf("unit not rendered or sent on a tty call: %q", unit.stdin)
	}
	// The cert goes to the staging area; deploy's cert call runs no sudo.
	if scp := readOr(t, filepath.Join(e.stub, "scp.argv")); !strings.Contains(scp, "jared@10.0.0.5:.cache/vmtool-deploy/stage/tls/") {
		t.Errorf("cert not staged: %q", scp)
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
	// GET /version: public, so no Authorization header at all, and no -k.
	curl := readOr(t, filepath.Join(e.stub, "curl.argv"))
	for _, w := range []string{"--fail", "https://veritech01.labtwo:9473/version", "--resolve veritech01.labtwo:9473:10.0.0.5"} {
		if !strings.Contains(curl, w) {
			t.Errorf("curl argv lacks %q: %q", w, curl)
		}
	}
	if strings.Contains(curl, "-k") || strings.Contains(curl, "--insecure") || strings.Contains(curl, "-H") {
		t.Errorf("curl skips verification or sends a header: %q", curl)
	}
	if got := readOr(t, filepath.Join(e.stub, "curl.stdin")); strings.Contains(got, "Authorization") || got != "" {
		t.Errorf("curl stdin %q", got)
	}
	if !strings.Contains(r.stdout, `{"version":"x"}`) {
		t.Errorf("version not printed: %q", r.stdout)
	}
	if _, err := os.Stat(filepath.Join(e.wd, "tls", "veritech01.labtwo.key")); err == nil {
		t.Error("local key kept")
	}
}

// HV_DIR moves the install; an old REMOTE_DIR is ignored (with a warning).
func TestDeployTokenFromDotEnvAndHVDir(t *testing.T) {
	e := newDeployEnv(t, "go1.22.2\n")
	e.withoutToken()
	os.WriteFile(filepath.Join(e.wd, ".env"), []byte("A=b\nVMTOOL_TOKEN="+deployTok+"\n"), 0o600)
	r := e.deploy(t, []string{"HV_DIR=/srv/vmtool", "REMOTE_DIR=/elsewhere"}, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	calls := sshCalls(t, e.stub)
	findCall(t, calls, "test -d /srv/vmtool")
	if i := findCall(t, calls, "sudo sh -c"); i >= 0 {
		for _, w := range []string{"/srv/vmtool/vmtool", "/srv/vmtool/config.yml", "/srv/vmtool/tls/veritech01.labtwo.key"} {
			if !strings.Contains(calls[i].argv, w) {
				t.Errorf("install ignores HV_DIR, lacks %q", w)
			}
		}
		if strings.Contains(calls[i].argv, "/opt/vmtool") {
			t.Errorf("install still targets /opt/vmtool: %q", calls[i].argv)
		}
	}
	if i := findCall(t, calls, "vmtool-daemon.service\""); i >= 0 && !strings.Contains(calls[i].stdin, "WorkingDirectory=/srv/vmtool") {
		t.Error("unit ignores HV_DIR")
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "ssh.argv"))+readOr(t, filepath.Join(e.stub, "scp.argv")), "/elsewhere") {
		t.Error("REMOTE_DIR used")
	}
	if !strings.Contains(r.stderr, "REMOTE_DIR") {
		t.Errorf("no warning that REMOTE_DIR is ignored: %q", r.stderr)
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "ssh.argv")), deployTok) {
		t.Error("token in argv")
	}
}

func TestDeployUnsafeHVDir(t *testing.T) {
	for _, d := range []string{"opt/vmtool", "/opt/vm tool", "/opt/$(id)", "/opt/vm'tool", `/opt/vm"tool`, "/opt/vm;tool", "/"} {
		t.Run(d, func(t *testing.T) {
			e := newDeployEnv(t, "go1.22.2\n")
			r := e.deploy(t, []string{"HV_DIR=" + d}, "veritech01")
			if r.code == 0 || !strings.Contains(r.stderr, "HV_DIR") {
				t.Fatalf("want HV_DIR refusal, got %d %q", r.code, r.stderr)
			}
			for _, f := range []string{"ssh.argv", "scp.argv", "tar.argv", "bao.argv"} {
				if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
					t.Errorf("%s called despite an unsafe HV_DIR", f)
				}
			}
		})
	}
}

// Without the vmtool user or HV_DIR, deploy stops after the Go check and
// before it copies, builds or touches OpenBao.
func TestDeployNeedsPrereqs(t *testing.T) {
	for _, c := range []struct{ name, exit, want string }{
		{"missing", "1", "run 'just prereqs veritech01' first"},
		{"ssh down", "255", "ssh to jared@10.0.0.5 failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newDeployEnv(t, "go1.22.2\n")
			os.WriteFile(filepath.Join(e.stub, "prereqexit"), []byte(c.exit), 0o644)
			r := e.deploy(t, nil, "veritech01")
			if r.code == 0 || !strings.Contains(r.stderr, c.want) {
				t.Fatalf("want %q, got %d %q", c.want, r.code, r.stderr)
			}
			for _, f := range []string{"tar.argv", "git.argv", "scp.argv", "bao.argv", "curl.argv"} {
				if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
					t.Errorf("%s called before the prereq check passed", f)
				}
			}
			calls := sshCalls(t, e.stub)
			if len(calls) != 2 || !strings.Contains(calls[0].argv, "go env GOVERSION") || !strings.Contains(calls[1].argv, "id vmtool") {
				t.Errorf("want go check then prereq check only: %+v", calls)
			}
		})
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

// An ssh user or address from config.yml that could be read as an ssh
// option, or that holds shell characters, is refused before any ssh.
func TestDeployRejectsOddHostEntry(t *testing.T) {
	for _, ans := range []string{"10.0.0.5 -oProxyCommand=x", "-oProxyCommand=x jared", "10.0.0.5 ja;red"} {
		t.Run(ans, func(t *testing.T) {
			e := newDeployEnv(t, "go1.22.2\n")
			addStub(t, e.stub, "vmtool", `echo "`+ans+`"`)
			r := e.deploy(t, nil, "veritech01")
			if r.code == 0 {
				t.Fatalf("exit 0 for %q", ans)
			}
			if _, err := os.Stat(filepath.Join(e.stub, "ssh.argv")); err == nil {
				t.Error("ssh called")
			}
		})
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

// Every external command must be found through PATH (so the tests' stubs
// replace it); an absolute path would reach the real binary.
func TestScriptsNoAbsoluteCommands(t *testing.T) {
	re := regexp.MustCompile(`(^|[\s;|&(!])/(usr/)?(local/)?s?bin/`)
	for _, f := range []string{"deploy.sh", "cert.sh"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(b), "\n") {
			if s := strings.TrimSpace(l); strings.HasPrefix(s, "#") {
				continue
			}
			// /usr/local/go/bin is put on the remote PATH, not executed.
			if re.MatchString(strings.ReplaceAll(l, "/usr/local/go/bin", "")) {
				t.Errorf("%s:%d runs a command by absolute path: %s", f, i+1, l)
			}
		}
	}
}

func TestJustDeployCertPassHVDir(t *testing.T) {
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just not installed")
	}
	for _, r := range []string{"deploy", "cert"} {
		out, err := exec.Command("just", "--justfile", "../justfile", "--dry-run", r, "h1").CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if !strings.Contains(string(out), `HV_DIR="/opt/vmtool"`) || strings.Contains(string(out), "REMOTE_DIR") {
			t.Errorf("%s recipe: %s", r, out)
		}
	}
}

// newRemoteEnv makes ssh and scp run the remote side for real, locally: each
// ssh command runs in bash with HOME set to a throwaway "remote home", and
// sudo, install, systemctl, id and go are stubs (install copies the file to
// <stub>/installed/<name> and logs "<dest> <mode>" to installed.list; the
// "built" vmtool writes a config.yml holding the token's hash). It checks
// that the remote command strings are valid shell and do what they say.
// `bash -lc` runs as `bash -c` and /usr/local/go/bin is rewritten to a
// missing dir, so no profile or real Go on this machine is involved.
func newRemoteEnv(t *testing.T) (e *certEnv, rhome, hv string) {
	t.Helper()
	e = newDeployEnv(t, "")
	rhome, hv = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(e.stub, "remotehome"), []byte(rhome), 0o644); err != nil {
		t.Fatal(err)
	}
	addStub(t, e.stub, "ssh", sshRecord+`[[ $1 == -t ]] && shift
shift
cmd="$*"
cmd=${cmd//bash -lc/bash -c}
cmd=${cmd//\/usr\/local\/go\/bin/$d/nogo}
cd "$(<"$d/remotehome")" && HOME="$PWD" bash -c "$cmd" < "$d/ssh.$n.stdin"`)
	addStub(t, e.stub, "scp", `files=()
for a; do [[ $a == -* ]] || files+=("$a"); done
dst=${files[-1]}; unset 'files[-1]'; dst=${dst#*:}
[[ $dst == /* ]] || dst="$(<"$d/remotehome")/$dst"
cp -p "${files[@]}" "$dst"`)
	addStub(t, e.stub, "sudo", `exec "$@"`)
	// `systemctl show -p MainPID --value <unit>` answers from mainpid.<unit>.
	addStub(t, e.stub, "systemctl", `if [[ $1 == show ]]; then f="$d/mainpid.${*: -1}"; [[ -f $f ]] && cat "$f"; exit 0; fi
exit 0`)
	// `ss` answers from ss.out (empty: nothing listens).
	addStub(t, e.stub, "ss", `[[ -f $d/ss.out ]] && cat "$d/ss.out"; exit 0`)
	addStub(t, e.stub, "id", `if [[ $1 == -un ]]; then echo tester; exit 0; fi
[[ $1 == vmtool ]]`)
	addStub(t, e.stub, "go", `if [[ "$1 $2" == "env GOVERSION" ]]; then echo go1.22.2; exit 0; fi
if [[ $1 == build && $2 == -o ]]; then
  cat > "$3" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$d/remote-vmtool.argv"
if [[ "\$1 \$2" == "config write" ]]; then
  [[ ! -e config.yml ]] || echo "config.yml not removed before config write" >> "$d/errors"
  IFS= read -r tok
  printf 'token_sha256: %s\n' "\$(printf %s "\$tok" | sha256sum | cut -d' ' -f1)" > config.yml
  stat -c %a config.yml > "$d/configmode"
fi
EOF
  chmod 755 "$3"
fi`)
	addStub(t, e.stub, "install", `mode=; dir=no; args=()
while [[ $# -gt 0 ]]; do
  case $1 in -o|-g) shift 2 ;; -m) mode=$2; shift 2 ;; -d) dir=yes; shift ;; *) args+=("$1"); shift ;; esac
done
[[ $dir == yes ]] && exit 0
[[ -f ${args[0]} ]] || { echo "install: no ${args[0]}" >&2; exit 1; }
mkdir -p "$d/installed"
cp "${args[0]}" "$d/installed/$(basename "${args[1]}")"
echo "${args[1]} $mode" >> "$d/installed.list"`)
	e.env = append(e.env, "HV_DIR="+hv, "UNIT_DIR="+t.TempDir(), "GO_BIN_DIR="+filepath.Join(e.stub, "nogo"))
	return e, rhome, hv
}

// get returns the value of an environment entry set for the script.
func (e *certEnv) get(key string) string {
	for _, v := range e.env {
		if strings.HasPrefix(v, key+"=") {
			return strings.TrimPrefix(v, key+"=")
		}
	}
	return ""
}

func fileMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestDeployRemoteCommandsRun(t *testing.T) {
	e, rhome, hv := newRemoteEnv(t)
	ud := e.get("UNIT_DIR")
	r := e.deploy(t, nil, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if errs := readOr(t, filepath.Join(e.stub, "errors")); errs != "" {
		t.Error(errs)
	}
	list := readOr(t, filepath.Join(e.stub, "installed.list"))
	want := hv + "/vmtool 0755\n" + hv + "/config.yml 0600\n" +
		hv + "/tls/veritech01.labtwo.crt 0644\n" + hv + "/tls/veritech01.labtwo.key 0600\n" +
		ud + "/vmtool-daemon.service 0644\n"
	if list != want {
		t.Errorf("installed:\n%s\nwant:\n%s", list, want)
	}
	sum := sha256.Sum256([]byte(deployTok))
	if got := readOr(t, filepath.Join(e.stub, "installed", "config.yml")); got != "token_sha256: "+hex.EncodeToString(sum[:])+"\n" {
		t.Errorf("installed config.yml %q", got)
	}
	if got := strings.TrimSpace(readOr(t, filepath.Join(e.stub, "configmode"))); got != "600" {
		t.Errorf("staged config.yml mode %s", got)
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "installed", "veritech01.labtwo.key")), "PRIVATE KEY") {
		t.Error("installed key is not a key")
	}
	u := readOr(t, filepath.Join(e.stub, "installed", "vmtool-daemon.service"))
	for _, w := range []string{"User=vmtool", "WorkingDirectory=" + hv, "ExecStart=" + hv + "/vmtool server"} {
		if !strings.Contains(u, w) {
			t.Errorf("unit lacks %q:\n%s", w, u)
		}
	}
	// The remote vmtool got the token on stdin only.
	if argv := readOr(t, filepath.Join(e.stub, "remote-vmtool.argv")); !strings.Contains(argv, "config write --token-stdin") {
		t.Errorf("remote vmtool argv %q", argv)
	}
	if got := strings.ReplaceAll(strings.TrimSpace(readOr(t, filepath.Join(e.stub, "systemctl.argv"))), "\n", ";"); got != "show -p MainPID --value vmtool-daemon;daemon-reload;enable --now vmtool-daemon;restart vmtool-daemon" {
		t.Errorf("systemctl calls %q", got)
	}
	if n := strings.Count(readOr(t, filepath.Join(e.stub, "sudo.argv")), "\n"); n != 1 {
		t.Errorf("sudo ran %d times, want 1", n)
	}
	// Staging: private dirs; config.yml and key gone after the install.
	base := filepath.Join(rhome, ".cache", "vmtool-deploy")
	for _, d := range []string{base, base + "/stage", base + "/stage/tls", base + "/src"} {
		if m := fileMode(t, d); m != 0o700 {
			t.Errorf("%s mode %v", d, m)
		}
	}
	for _, f := range []string{"stage/config.yml", "stage/tls/veritech01.labtwo.key"} {
		if _, err := os.Stat(filepath.Join(base, f)); err == nil {
			t.Errorf("staged %s not removed", f)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "stage", "vmtool")); err != nil {
		t.Errorf("binary not built into stage: %v", err)
	}
	tokenNowhereBut(t, findCall(t, sshCalls(t, e.stub), "config write")+1, e.stub, e.wd, rhome, hv)
	if _, err := os.Stat(filepath.Join(e.wd, "tls", "veritech01.labtwo.key")); err == nil {
		t.Error("local key kept")
	}
}

// A failed sudo install still removes the staged config.yml and key, and
// deploy fails.
func TestDeployInstallFailureCleansStage(t *testing.T) {
	e, rhome, _ := newRemoteEnv(t)
	addStub(t, e.stub, "sudo", "exit 1")
	r := e.deploy(t, nil, "veritech01")
	if r.code == 0 {
		t.Fatal("exit 0 although sudo failed")
	}
	base := filepath.Join(rhome, ".cache", "vmtool-deploy", "stage")
	for _, f := range []string{"config.yml", "tls/veritech01.labtwo.key"} {
		if _, err := os.Stat(filepath.Join(base, f)); err == nil {
			t.Errorf("staged %s kept after a failed install", f)
		}
	}
	if _, err := os.Stat(filepath.Join(e.stub, "curl.argv")); err == nil {
		t.Error("curl after a failed install")
	}
}

// The prereq check runs for real: HV_DIR must exist.
func TestDeployRemotePrereqMissingDir(t *testing.T) {
	e, _, hv := newRemoteEnv(t)
	r := e.deploy(t, []string{"HV_DIR=" + hv + "/nope"}, "veritech01")
	if r.code == 0 || !strings.Contains(r.stderr, "just prereqs veritech01") {
		t.Fatalf("exit %d: %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.stub, "tar.argv")); err == nil {
		t.Error("copied despite missing HV_DIR")
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

// A listed host's port is the one the daemon listens on, the one curl asks,
// and the one the firewall rule gets (see TestPrereqsHostPort).
func TestDeployListedPort(t *testing.T) {
	e := newDeployEnv(t, "go1.22.2\n")
	addStub(t, e.stub, "vmtool", `if [[ "$1 $2" == "config host" ]]; then echo "10.0.0.5 jared 9474"; fi`)
	r := e.deploy(t, nil, "PLACEHOLDER-HOST")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	calls := sshCalls(t, e.stub)
	findCall(t, calls, "--listen 0.0.0.0:9474 --tls-cert tls/PLACEHOLDER-HOST.labtwo.crt")
	if curl := readOr(t, filepath.Join(e.stub, "curl.argv")); !strings.Contains(curl, "https://PLACEHOLDER-HOST.labtwo:9474/version") ||
		!strings.Contains(curl, "--resolve PLACEHOLDER-HOST.labtwo:9474:10.0.0.5") {
		t.Errorf("curl argv %q", curl)
	}
}

// newLocalEnv: deploy.sh run for this machine, which the `hostname` stub names
// PLACEHOLDER-HOST (config.yml has no name:). Every command in the local path
// is a stub or a harmless coreutil; any ssh call records ssh.argv and fails.
// listed is the `config host` answer, or "" for an unlisted entry (the stub
// vmtool then fails with "unknown host", as the real one does).
func newLocalEnv(t *testing.T, listed string) (e *certEnv, hv, ud string) {
	t.Helper()
	e, _, hv = newRemoteEnv(t)
	ud = e.get("UNIT_DIR")
	if err := os.WriteFile(filepath.Join(e.wd, "config.yml"), []byte("role: workstation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addStub(t, e.stub, "hostname", "echo PLACEHOLDER-HOST")
	addStub(t, e.stub, "ssh", "exit 99")
	addStub(t, e.stub, "scp", "exit 99")
	if listed != "" {
		if err := os.WriteFile(filepath.Join(e.stub, "listed"), []byte(listed+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	addStub(t, e.stub, "vmtool", `echo "vmtool $*" >> "$d/order"
if [[ "$1 $2" == "config host" ]]; then
  if [[ -f $d/listed ]]; then cat "$d/listed"; exit 0; fi
  echo 'Error: unknown host "PLACEHOLDER-HOST" in config.yml' >&2; exit 1
fi`)
	addStub(t, e.stub, "curl", `cat > "$d/curl.stdin"; echo curl >> "$d/order"; [[ -f $d/curlfail ]] && exit 22; echo '{"version":"x"}'`)
	return e, hv, ud
}

func (e *certEnv) systemctlCalls(t *testing.T) string {
	return strings.ReplaceAll(strings.TrimSpace(readOr(t, filepath.Join(e.stub, "systemctl.argv"))), "\n", ";")
}

func TestDeployLocalNoSSHOneSudo(t *testing.T) {
	e, hv, ud := newLocalEnv(t, "10.0.0.9 ops 9475")
	r := e.deploy(t, nil, "PLACEHOLDER-HOST")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	for _, f := range []string{"ssh.argv", "scp.argv"} {
		if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
			t.Errorf("%s called for this machine", f)
		}
	}
	if n := strings.Count(readOr(t, filepath.Join(e.stub, "sudo.argv")), "\n"); n != 1 {
		t.Errorf("sudo ran %d times, want 1", n)
	}
	if errs := readOr(t, filepath.Join(e.stub, "errors")); errs != "" {
		t.Error(errs)
	}
	// Listed: the entry's port, listen on all interfaces, curl there.
	if argv := readOr(t, filepath.Join(e.stub, "remote-vmtool.argv")); !strings.Contains(argv, "config write --token-stdin --role hypervisor --name PLACEHOLDER-HOST --listen 0.0.0.0:9475 --tls-cert tls/PLACEHOLDER-HOST.labtwo.crt") {
		t.Errorf("config write argv %q", argv)
	}
	curl := readOr(t, filepath.Join(e.stub, "curl.argv"))
	if !strings.Contains(curl, "https://PLACEHOLDER-HOST.labtwo:9475/version") || !strings.Contains(curl, "--resolve PLACEHOLDER-HOST.labtwo:9475:10.0.0.9") {
		t.Errorf("curl argv %q", curl)
	}
	// ss was asked about that port.
	if ss := readOr(t, filepath.Join(e.stub, "ss.argv")); !strings.Contains(ss, "sport = :9475") {
		t.Errorf("ss argv %q", ss)
	}
	want := hv + "/vmtool 0755\n" + hv + "/config.yml 0600\n" +
		hv + "/tls/PLACEHOLDER-HOST.labtwo.crt 0644\n" + hv + "/tls/PLACEHOLDER-HOST.labtwo.key 0600\n" +
		ud + "/vmtool-daemon.service 0644\n"
	if got := readOr(t, filepath.Join(e.stub, "installed.list")); got != want {
		t.Errorf("installed:\n%s\nwant:\n%s", got, want)
	}
	if got := e.systemctlCalls(t); got != "show -p MainPID --value vmtool-daemon;daemon-reload;enable --now vmtool-daemon;restart vmtool-daemon" {
		t.Errorf("systemctl calls %q", got)
	}
	// The checkout's own tls/ pair stays (the loopback unit uses it); the
	// staged key is gone.
	if _, err := os.Stat(filepath.Join(e.wd, "tls", "PLACEHOLDER-HOST.labtwo.key")); err != nil {
		t.Errorf("checkout key removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.wd, ".cache", "vmtool-deploy", "stage", "tls", "PLACEHOLDER-HOST.labtwo.key")); err == nil {
		t.Error("staged key kept")
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "installed", "PLACEHOLDER-HOST.labtwo.key")), "PRIVATE KEY") {
		t.Error("installed key is not a key")
	}
	// A listed entry is never rewritten.
	if strings.Contains(readOr(t, filepath.Join(e.stub, "vmtool.argv")), "set-host") {
		t.Error("set-host run for a listed entry")
	}
	tokenNowhereBut(t, 0, e.stub, e.wd, hv)
}

func TestDeployLocalUnlistedSetsHost(t *testing.T) {
	e, _, _ := newLocalEnv(t, "")
	r := e.deploy(t, nil, "PLACEHOLDER-HOST")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.stub, "ssh.argv")); err == nil {
		t.Error("ssh called")
	}
	if curl := readOr(t, filepath.Join(e.stub, "curl.argv")); !strings.Contains(curl, "https://PLACEHOLDER-HOST.labtwo:9474/version") || strings.Contains(curl, "--resolve") {
		t.Errorf("curl argv %q", curl)
	}
	if argv := readOr(t, filepath.Join(e.stub, "remote-vmtool.argv")); !strings.Contains(argv, "--listen 0.0.0.0:9474 ") {
		t.Errorf("config write argv %q", argv)
	}
	// set-host comes after the good /version, with the defaults.
	lines := strings.Split(strings.TrimSpace(readOr(t, filepath.Join(e.stub, "order"))), "\n")
	last := lines[len(lines)-1]
	if last != "vmtool config set-host PLACEHOLDER-HOST --address PLACEHOLDER-HOST.labtwo --ssh-user tester --port 9474" || lines[len(lines)-2] != "curl" {
		t.Errorf("order %q", lines)
	}
	if n := strings.Count(strings.Join(lines, "\n"), "set-host"); n != 1 {
		t.Errorf("set-host ran %d times", n)
	}
}

func TestDeployLocalUnlistedSelfOverrides(t *testing.T) {
	e, _, _ := newLocalEnv(t, "")
	r := e.deploy(t, []string{"VMTOOL_SELF_ADDRESS=10.0.0.7", "VMTOOL_SELF_PORT=9480"}, "PLACEHOLDER-HOST")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if curl := readOr(t, filepath.Join(e.stub, "curl.argv")); !strings.Contains(curl, "--resolve PLACEHOLDER-HOST.labtwo:9480:10.0.0.7") {
		t.Errorf("curl argv %q", curl)
	}
	if got := readOr(t, filepath.Join(e.stub, "vmtool.argv")); !strings.Contains(got, "config set-host PLACEHOLDER-HOST --address 10.0.0.7 --ssh-user tester --port 9480") {
		t.Errorf("vmtool argv %q", got)
	}
}

// A /version that never answers: the daemon is installed but the entry is
// not written.
func TestDeployLocalFailedVersionNoSetHost(t *testing.T) {
	e, _, _ := newLocalEnv(t, "")
	os.WriteFile(filepath.Join(e.stub, "curlfail"), nil, 0o644)
	r := e.deploy(t, nil, "PLACEHOLDER-HOST")
	if r.code == 0 || !strings.Contains(r.stderr, "systemctl status vmtool-daemon") {
		t.Fatalf("exit %d: %q", r.code, r.stderr)
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "vmtool.argv")), "set-host") {
		t.Error("set-host run after a failed /version")
	}
}

// Any `config host` failure other than the own name being unlisted is fatal,
// and a name that is not this machine's never takes the unlisted path.
func TestDeployConfigHostFailureIsFatal(t *testing.T) {
	e, _, _ := newLocalEnv(t, "")
	addStub(t, e.stub, "vmtool", `echo "Error: boom" >&2; exit 1`)
	if r := e.deploy(t, nil, "PLACEHOLDER-HOST"); r.code == 0 || !strings.Contains(r.stderr, "boom") {
		t.Errorf("local, other failure: exit %d %q", r.code, r.stderr)
	}
	e, _, _ = newLocalEnv(t, "")
	if r := e.deploy(t, nil, "hv1"); r.code == 0 || !strings.Contains(r.stderr, "unknown host") {
		t.Errorf("remote, unlisted: exit %d %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.stub, "ssh.argv")); err == nil {
		t.Error("ssh called for an unlisted remote host")
	}
}

const (
	foreignListener = `LISTEN 0 4096 0.0.0.0:9474 0.0.0.0:* users:(("nginx",pid=4242,fd=6))` + "\n"
	ourListener     = `LISTEN 0 4096 0.0.0.0:9474 0.0.0.0:* users:(("vmtool",pid=777,fd=7))` + "\n"
)

func writeStub(t *testing.T, e *certEnv, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.stub, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The unit is vmtool-daemon.service. An older vmtool.service run from HV_DIR
// is disabled, stopped and removed in the same sudo script; one run from
// anywhere else is left alone.
func TestDeployUnitOldVmtoolServiceRemoved(t *testing.T) {
	e, hv, ud := newLocalEnv(t, "10.0.0.9 ops 9474")
	old := filepath.Join(ud, "vmtool.service")
	os.WriteFile(old, []byte("[Service]\nWorkingDirectory="+hv+"\nExecStart="+hv+"/vmtool server\n"), 0o644)
	r := e.deploy(t, nil, "PLACEHOLDER-HOST")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("old vmtool.service not removed")
	}
	got := e.systemctlCalls(t)
	if !strings.Contains(got, "disable --now vmtool;daemon-reload;enable --now vmtool-daemon;restart vmtool-daemon") {
		t.Errorf("systemctl calls %q", got)
	}
	if n := strings.Count(readOr(t, filepath.Join(e.stub, "sudo.argv")), "\n"); n != 1 {
		t.Errorf("sudo ran %d times, want 1", n)
	}
}

func TestDeployUnitOtherVmtoolServiceLeftAlone(t *testing.T) {
	e, _, ud := newLocalEnv(t, "10.0.0.9 ops 9474")
	other := filepath.Join(ud, "vmtool.service")
	os.WriteFile(other, []byte("[Service]\nWorkingDirectory=/home/someone/vmtool\n"), 0o644)
	r := e.deploy(t, nil, "PLACEHOLDER-HOST")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("other vmtool.service removed: %v", err)
	}
	if got := e.systemctlCalls(t); strings.Contains(got, "disable") || strings.Contains(got, "--now vmtool;") {
		t.Errorf("systemctl calls %q", got)
	}
}

// A port held by anything but our daemon stops the install before any
// change: nothing installed, no systemctl change, the old unit still there.
func TestDeployRefusesForeignListener(t *testing.T) {
	e, hv, ud := newLocalEnv(t, "10.0.0.9 ops 9474")
	old := filepath.Join(ud, "vmtool.service")
	os.WriteFile(old, []byte("WorkingDirectory="+hv+"\n"), 0o644)
	writeStub(t, e, "ss.out", foreignListener)
	writeStub(t, e, "mainpid.vmtool-daemon", "777\n")
	r := e.deploy(t, nil, "PLACEHOLDER-HOST")
	if r.code == 0 || !strings.Contains(r.stderr, "9474") || !strings.Contains(r.stderr, "4242") {
		t.Fatalf("want a refusal naming the port and pid, got %d %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.stub, "installed.list")); err == nil {
		t.Error("files installed despite the refusal")
	}
	if got := e.systemctlCalls(t); strings.Contains(got, "daemon-reload") || strings.Contains(got, "enable") || strings.Contains(got, "disable") {
		t.Errorf("systemctl changed something: %q", got)
	}
	if _, err := os.Stat(old); err != nil {
		t.Error("old unit removed despite the refusal")
	}
	if _, err := os.Stat(filepath.Join(e.stub, "curl.argv")); err == nil {
		t.Error("curl after a refused install")
	}
	// An unidentifiable holder is refused too.
	writeStub(t, e, "ss.out", "LISTEN 0 4096 0.0.0.0:9474 0.0.0.0:*\n")
	if r := e.deploy(t, nil, "PLACEHOLDER-HOST"); r.code == 0 {
		t.Error("unidentified listener accepted")
	}
}

func TestDeployAcceptsOurOwnListener(t *testing.T) {
	e, _, _ := newLocalEnv(t, "10.0.0.9 ops 9474")
	writeStub(t, e, "ss.out", ourListener)
	writeStub(t, e, "mainpid.vmtool-daemon", "777\n")
	if r := e.deploy(t, nil, "PLACEHOLDER-HOST"); r.code != 0 {
		t.Fatalf("our own daemon refused: exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(e.systemctlCalls(t), "restart vmtool-daemon") {
		t.Errorf("not restarted: %q", e.systemctlCalls(t))
	}
}

// The older vmtool.service from HV_DIR is ours as well: its MainPID may hold
// the port while it is replaced.
func TestDeployAcceptsOldUnitListener(t *testing.T) {
	e, hv, ud := newLocalEnv(t, "10.0.0.9 ops 9474")
	os.WriteFile(filepath.Join(ud, "vmtool.service"), []byte("WorkingDirectory="+hv+"\n"), 0o644)
	writeStub(t, e, "ss.out", `LISTEN 0 4096 0.0.0.0:9474 0.0.0.0:* users:(("vmtool",pid=555,fd=7))`+"\n")
	writeStub(t, e, "mainpid.vmtool", "555\n")
	if r := e.deploy(t, nil, "PLACEHOLDER-HOST"); r.code != 0 {
		t.Fatalf("old unit's daemon refused: exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	// ... but a vmtool.service from another directory is not.
	e, _, ud = newLocalEnv(t, "10.0.0.9 ops 9474")
	os.WriteFile(filepath.Join(ud, "vmtool.service"), []byte("WorkingDirectory=/home/someone/vmtool\n"), 0o644)
	writeStub(t, e, "ss.out", `LISTEN 0 4096 0.0.0.0:9474 0.0.0.0:* users:(("vmtool",pid=555,fd=7))`+"\n")
	writeStub(t, e, "mainpid.vmtool", "555\n")
	if r := e.deploy(t, nil, "PLACEHOLDER-HOST"); r.code == 0 {
		t.Error("a foreign vmtool.service's daemon was treated as ours")
	}
}

// storeURLStub is the local vmtool stub for the store-url tests: `config
// host` answers a host entry, `config server-url` prints what the test put in
// the file serverurl (nothing when absent, like a client without a store).
const storeURLStub = `if [[ "$1 $2" == "config host" ]]; then echo "10.0.0.5 jared 9473"; fi
if [[ "$1 $2" == "config server-url" && -f $d/serverurl ]]; then cat "$d/serverurl"; fi`

func configWriteCall(t *testing.T, e *certEnv) sshCall {
	t.Helper()
	calls := sshCalls(t, e.stub)
	return calls[findCall(t, calls, "config write --token-stdin")]
}

// A client with server.url has its daemon told: --store-url on the config
// write call, with that URL. The token still rides stdin alone.
func TestDeployPassesStoreURL(t *testing.T) {
	e := newDeployEnv(t, "go1.22.2\n")
	addStub(t, e.stub, "vmtool", storeURLStub)
	writeStub(t, e, "serverurl", "https://PLACEHOLDER-HOST:8095\n")
	if r := e.deploy(t, nil, "veritech01"); r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	cfg := configWriteCall(t, e)
	if !strings.Contains(cfg.argv, "--tls-key tls/veritech01.labtwo.key --store-url https://PLACEHOLDER-HOST:8095") {
		t.Errorf("config write call: %q", cfg.argv)
	}
	if cfg.stdin != deployTok+"\n" || strings.Contains(cfg.argv, deployTok) {
		t.Errorf("token placement: argv %q stdin %q", cfg.argv, cfg.stdin)
	}
}

// A client without server.url: no --store-url anywhere, the call is exactly
// the one it was before.
func TestDeployNoStoreURLWhenClientHasNone(t *testing.T) {
	e := newDeployEnv(t, "go1.22.2\n")
	addStub(t, e.stub, "vmtool", storeURLStub)
	if r := e.deploy(t, nil, "veritech01"); r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	for i, c := range sshCalls(t, e.stub) {
		if strings.Contains(c.argv, "store-url") {
			t.Errorf("ssh call %d mentions store-url: %q", i+1, c.argv)
		}
	}
	if !strings.HasSuffix(configWriteCall(t, e).argv, "--tls-key tls/veritech01.labtwo.key") {
		t.Errorf("config write call changed: %q", configWriteCall(t, e).argv)
	}
}

// The URL lands in a remote shell command: anything but a plain https URL is
// refused before any ssh call.
func TestDeployRefusesUnsafeStoreURL(t *testing.T) {
	for _, u := range []string{"https://x;touch /tmp/PLACEHOLDER", "https://x'y", "http://PLACEHOLDER-HOST", "https://x y", "https://$(id)"} {
		e := newDeployEnv(t, "go1.22.2\n")
		addStub(t, e.stub, "vmtool", storeURLStub)
		writeStub(t, e, "serverurl", u+"\n")
		r := e.deploy(t, nil, "veritech01")
		if r.code == 0 || !strings.Contains(r.stderr, "server.url") {
			t.Errorf("%q: exit %d: %s%s", u, r.code, r.stdout, r.stderr)
		}
		if _, err := os.Stat(filepath.Join(e.stub, "ssh.argv")); err == nil {
			t.Errorf("%q: ssh ran", u)
		}
	}
}

// On this machine the same flag goes to the local config write.
func TestDeployLocalPassesStoreURL(t *testing.T) {
	e, _, _ := newLocalEnv(t, "10.0.0.9 ops 9475")
	addStub(t, e.stub, "vmtool", `echo "vmtool $*" >> "$d/order"
if [[ "$1 $2" == "config host" ]]; then cat "$d/listed"; fi
if [[ "$1 $2" == "config server-url" ]]; then echo https://PLACEHOLDER-HOST:8095; fi`)
	if r := e.deploy(t, nil, "PLACEHOLDER-HOST"); r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if argv := readOr(t, filepath.Join(e.stub, "remote-vmtool.argv")); !strings.Contains(argv, "--store-url https://PLACEHOLDER-HOST:8095") {
		t.Errorf("config write argv %q", argv)
	}
}
