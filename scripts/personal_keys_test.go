package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	keysListURL = "https://keys.example.lab/keys/v1/lists/example"
	keysReqURL  = "https://keys.example.lab/v1/services/example/requests"
	keysPath    = "kv/example/keys/pull/vmtool"
	pullTok     = "pullpullpullpullpullpullpullpullpullpullpullpullpullpullpullpull"

	// Today's config write argv on the daemon (hv1, no store, no network home).
	todayDaemonConfigWrite = "config write --token-stdin --role hypervisor --name hv1 --listen 0.0.0.0:9473 --tls-cert tls/hv1.example.lab.crt --tls-key tls/hv1.example.lab.key"
)

// keysBao makes the stub bao answer the CA fields (cert.sh) and the pull
// token: `kv get -field=token kv/example/keys/pull/vmtool` prints the file
// pulltok in the stub dir, and fails when that file is absent.
func keysBao(t *testing.T, e *certEnv, withToken string) {
	t.Helper()
	addStub(t, e.stub, "bao", `if [[ "$1 $2" == "kv get" ]]; then
  case "$3 $4" in
    "-field=string "*/crt) cat '`+e.caCrt+`' ;;
    "-field=string "*/key) cat '`+e.caKey+`' ;;
    "-field=token `+keysPath+`") [[ -f $d/pulltok ]] || { echo "No value found" >&2; exit 2; }; cat "$d/pulltok" ;;
    *) exit 3 ;;
  esac
fi`)
	if withToken != "" {
		writeStub(t, e, "pulltok", withToken)
	}
}

func keysEnv(t *testing.T, tok string) (e *certEnv, rhome, hv string) {
	t.Helper()
	e, rhome, hv = newRemoteEnv(t)
	keysBao(t, e, tok)
	e.env = append(e.env, "VMTOOL_DOMAIN=example.lab")
	return
}

var keysSet = []string{"VMTOOL_PERSONAL_KEYS_URL=" + keysListURL, "BAO_KEYS_PULL_TOKEN_PATH=" + keysPath}

// noPullLeak fails if the pull token or the operator token is in the run's
// output, in any ssh argv, or in any recorded file but the stdin of the ssh
// calls, the installed copies and bao's own answer file. The operator token
// may only be on the stdin of the config write call.
func noPullLeak(t *testing.T, e *certEnv, r result) {
	t.Helper()
	for _, tok := range []string{pullTok, deployTok} {
		if strings.Contains(r.stdout+r.stderr, tok) {
			t.Errorf("a token is in the output: %s%s", r.stdout, r.stderr)
		}
		for _, c := range sshCalls(t, e.stub) {
			if strings.Contains(c.argv, tok) {
				t.Errorf("a token is in an ssh argv %q", c.argv)
			}
		}
	}
	filepath.Walk(e.stub, func(p string, fi os.FileInfo, err error) error {
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		b := filepath.Base(p)
		if strings.HasSuffix(b, ".stdin") || filepath.Base(filepath.Dir(p)) == "installed" || b == "pulltok" {
			return nil
		}
		if d, _ := os.ReadFile(p); strings.Contains(string(d), pullTok) || strings.Contains(string(d), deployTok) {
			t.Errorf("a token is in %s", p)
		}
		return nil
	})
	for _, c := range sshCalls(t, e.stub) {
		if strings.Contains(c.stdin, pullTok) && !strings.Contains(c.argv, "keys-pull-token") {
			t.Errorf("the pull token rides an ssh call that is not its own: %q", c.argv)
		}
	}
}

func TestDeployPersonalKeysUnsetIsToday(t *testing.T) {
	e, _, hv := keysEnv(t, pullTok+"\n")
	// Even with a path and a request-less environment, no URL: nothing is read or passed.
	r := e.deploy(t, []string{"BAO_KEYS_PULL_TOKEN_PATH=" + keysPath}, "hv1")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if got := strings.TrimSpace(readOr(t, filepath.Join(e.stub, "remote-vmtool.argv"))); got != todayDaemonConfigWrite {
		t.Errorf("config write argv:\n got %q\nwant %q", got, todayDaemonConfigWrite)
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "bao.argv")), "-field=token") {
		t.Errorf("bao asked for a pull token: %q", readOr(t, filepath.Join(e.stub, "bao.argv")))
	}
	for _, c := range sshCalls(t, e.stub) {
		if strings.Contains(c.argv, "keys-pull-token") || strings.Contains(c.argv, "personal") {
			t.Errorf("ssh argv names the personal keys: %q", c.argv)
		}
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "installed.list")), "keys-pull-token") {
		t.Error("a pull token was installed")
	}
	if _, err := os.Stat(filepath.Join(hv, "keys-pull-token")); err == nil {
		t.Error("keys-pull-token exists")
	}
}

func TestDeployPersonalKeysSet(t *testing.T) {
	e, rhome, hv := keysEnv(t, pullTok+"\n")
	r := e.deploy(t, append([]string{"VMTOOL_KEY_REQUEST_URL=" + keysReqURL}, keysSet...), "hv1")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	want := todayDaemonConfigWrite + " --personal-keys-url " + keysListURL +
		" --personal-keys-token-file keys-pull-token --personal-keys-cache-file personal-keys.json --key-request-url " + keysReqURL
	if got := strings.TrimSpace(readOr(t, filepath.Join(e.stub, "remote-vmtool.argv"))); got != want {
		t.Errorf("config write argv:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "bao.argv")), "kv get -field=token "+keysPath+"\n") {
		t.Errorf("bao argv: %q", readOr(t, filepath.Join(e.stub, "bao.argv")))
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "installed.list")), hv+"/keys-pull-token 0600\n") {
		t.Errorf("installed: %s", readOr(t, filepath.Join(e.stub, "installed.list")))
	}
	if got := readOr(t, filepath.Join(e.stub, "installed", "keys-pull-token")); strings.TrimSpace(got) != pullTok {
		t.Errorf("installed pull token has the wrong content (len %d)", len(got))
	}
	if _, err := os.Stat(filepath.Join(rhome, ".cache", "vmtool-deploy", "stage", "keys-pull-token")); err == nil {
		t.Error("staged keys-pull-token not removed")
	}
	carriers := 0
	for _, c := range sshCalls(t, e.stub) {
		if strings.Contains(c.stdin, pullTok) {
			carriers++
			if !strings.Contains(c.argv, "umask 077") {
				t.Errorf("pull token call argv %q", c.argv)
			}
		}
	}
	if carriers != 1 {
		t.Errorf("%d ssh calls carry the pull token, want 1", carriers)
	}
	noPullLeak(t, e, r)
}

// A request URL alone is passed on its own: no list, no pull token read.
func TestDeployKeyRequestURLAlone(t *testing.T) {
	e, _, _ := keysEnv(t, pullTok+"\n")
	r := e.deploy(t, []string{"VMTOOL_KEY_REQUEST_URL=" + keysReqURL}, "hv1")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	want := todayDaemonConfigWrite + " --key-request-url " + keysReqURL
	if got := strings.TrimSpace(readOr(t, filepath.Join(e.stub, "remote-vmtool.argv"))); got != want {
		t.Errorf("config write argv:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "bao.argv")), "-field=token") {
		t.Error("bao asked for a pull token")
	}
}

// A URL with no pull token to install is fatal before anything is copied,
// installed or restarted; the old daemon's files stay.
func TestDeployPersonalKeysMissingPullTokenIsFatal(t *testing.T) {
	for _, tc := range []struct {
		name string
		tok  string
		env  []string
		want string
	}{
		{"absent", "", keysSet, "no pull token"},
		{"empty", "\n", keysSet, "no pull token"},
		{"path unset", pullTok, []string{"VMTOOL_PERSONAL_KEYS_URL=" + keysListURL}, "BAO_KEYS_PULL_TOKEN_PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, hv := keysEnv(t, tc.tok)
			old := filepath.Join(hv, "config.yml")
			if err := os.WriteFile(old, []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			r := e.deploy(t, tc.env, "hv1")
			if r.code == 0 || !strings.Contains(r.stderr, tc.want) || !strings.Contains(r.stderr, "hv1") {
				t.Fatalf("exit %d stderr %q, want a failure naming %q", r.code, r.stderr, tc.want)
			}
			for _, f := range []string{"systemctl.argv", "installed.list", "ssh.argv", "sudo.argv"} {
				if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
					t.Errorf("%s exists: the run went on", f)
				}
			}
			if got := readOr(t, old); got != "old\n" {
				t.Errorf("the old daemon's config changed: %q", got)
			}
			noPullLeak(t, e, r)
		})
	}
}

func TestDeployLocalPersonalKeys(t *testing.T) {
	e, hv, _ := newLocalEnv(t, "")
	e.env = append(e.env, "VMTOOL_DOMAIN=example.lab")
	keysBao(t, e, pullTok+"\n")
	e.env = append(e.env, "VMTOOL_SELF_ADDRESS=127.0.0.1", "VMTOOL_KEY_REQUEST_URL="+keysReqURL)
	r := e.deploy(t, keysSet, "PLACEHOLDER-HOST")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv := readOr(t, filepath.Join(e.stub, "remote-vmtool.argv"))
	for _, w := range []string{"--personal-keys-url " + keysListURL, "--personal-keys-token-file keys-pull-token",
		"--personal-keys-cache-file personal-keys.json", "--key-request-url " + keysReqURL} {
		if !strings.Contains(argv, w) {
			t.Errorf("config write argv lacks %q: %q", w, argv)
		}
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "installed.list")), hv+"/keys-pull-token 0600\n") {
		t.Errorf("installed: %s", readOr(t, filepath.Join(e.stub, "installed.list")))
	}
	if _, err := os.Stat(filepath.Join(e.stub, "ssh.argv")); err == nil {
		t.Error("ssh was called on this machine")
	}
	noPullLeak(t, e, r)
}

// The failed install still removes the staged pull token.
func TestDeployInstallFailureRemovesStagedPullToken(t *testing.T) {
	e, rhome, _ := keysEnv(t, pullTok+"\n")
	addStub(t, e.stub, "sudo", `exit 1`)
	r := e.deploy(t, keysSet, "hv1")
	if r.code == 0 {
		t.Fatal("deploy succeeded")
	}
	if _, err := os.Stat(filepath.Join(rhome, ".cache", "vmtool-deploy", "stage", "keys-pull-token")); err == nil {
		t.Error("staged keys-pull-token kept after a failed install")
	}
	noPullLeak(t, e, r)
}

// No VMTOOL_TOKEN anywhere: fail by name, telling the operator to export it,
// before any ssh, build or restart; .env is never written.
func TestDeployNoTokenSaysExport(t *testing.T) {
	e, _, _ := keysEnv(t, pullTok+"\n")
	e.withoutToken()
	r := e.deploy(t, keysSet, "hv1")
	if r.code == 0 || !strings.Contains(r.stderr, "VMTOOL_TOKEN") || !strings.Contains(r.stderr, "export") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	for _, f := range []string{"ssh.argv", "systemctl.argv", "sudo.argv", "bao.argv", "installed.list"} {
		if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
			t.Errorf("%s exists: the run went on without a token", f)
		}
	}
	if _, err := os.Stat(filepath.Join(e.wd, ".env")); err == nil {
		t.Error(".env was written")
	}
}

func writeDeployEnvFile(t *testing.T, e *certEnv, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.wd, "deploy.env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// deploy.env supplies the three settings; the environment wins over it.
func TestDeployEnvFile(t *testing.T) {
	e, _, hv := keysEnv(t, pullTok+"\n")
	writeDeployEnvFile(t, e, "# the key list\n\nVMTOOL_PERSONAL_KEYS_URL=\"https://keys.example.lab/from-file\"\r\n  # indented comment\nBAO_KEYS_PULL_TOKEN_PATH="+keysPath+"\nVMTOOL_KEY_REQUEST_URL='"+keysReqURL+"'")
	r := e.deploy(t, nil, "hv1")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv := readOr(t, filepath.Join(e.stub, "remote-vmtool.argv"))
	for _, w := range []string{"--personal-keys-url https://keys.example.lab/from-file ", "--key-request-url " + keysReqURL} {
		if !strings.Contains(argv, w) {
			t.Errorf("config write argv lacks %q: %q", w, argv)
		}
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "installed.list")), hv+"/keys-pull-token 0600\n") {
		t.Error("pull token not installed")
	}

	// The environment wins: the list URL from it, the rest from the file.
	e, _, _ = keysEnv(t, pullTok+"\n")
	writeDeployEnvFile(t, e, "VMTOOL_PERSONAL_KEYS_URL=https://keys.example.lab/from-file\nBAO_KEYS_PULL_TOKEN_PATH="+keysPath+"\n")
	r = e.deploy(t, []string{"VMTOOL_PERSONAL_KEYS_URL=" + keysListURL}, "hv1")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv = readOr(t, filepath.Join(e.stub, "remote-vmtool.argv"))
	if !strings.Contains(argv, "--personal-keys-url "+keysListURL+" ") || strings.Contains(argv, "from-file") {
		t.Errorf("the environment did not win: %q", argv)
	}
	noPullLeak(t, e, r)
}

// A line that is not one of the three settings stops the run, naming the line
// number and never its content, before anything is called.
func TestDeployEnvFileBadLine(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"other key", "VMTOOL_TOKEN=SECRETSECRET\n"},
		{"no equals", "BAO_KEYS_PULL_TOKEN_PATH\n"},
		{"export prefix", "export VMTOOL_PERSONAL_KEYS_URL=" + keysListURL + "\n"},
		{"command", "$(touch pwned)\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, _ := keysEnv(t, pullTok+"\n")
			writeDeployEnvFile(t, e, "# header\n\n"+tc.body)
			r := e.deploy(t, nil, "hv1")
			if r.code == 0 || !strings.Contains(r.stderr, "deploy.env line 3") {
				t.Fatalf("exit %d stderr %q, want a failure naming line 3", r.code, r.stderr)
			}
			for _, bad := range []string{"SECRETSECRET", "pwned", "touch"} {
				if strings.Contains(r.stderr, bad) {
					t.Errorf("stderr echoes the line: %q", r.stderr)
				}
			}
			if _, err := os.Stat(filepath.Join(e.stub, "ssh.argv")); err == nil {
				t.Error("ssh called after a bad deploy.env")
			}
			if _, err := os.Stat(filepath.Join(e.wd, "pwned")); err == nil {
				t.Error("deploy.env was executed")
			}
		})
	}
}

func TestDeployRefusesUnsafeKeysSettings(t *testing.T) {
	for _, env := range [][]string{
		{"VMTOOL_PERSONAL_KEYS_URL=https://keys.example.lab/x;touch$IFS/tmp/p"},
		{"VMTOOL_KEY_REQUEST_URL=https://user:pw@keys.example.lab/r"},
		{"VMTOOL_PERSONAL_KEYS_URL=" + keysListURL, "BAO_KEYS_PULL_TOKEN_PATH=-field=x"},
	} {
		e, _, _ := keysEnv(t, pullTok+"\n")
		r := e.deploy(t, env, "hv1")
		if r.code == 0 {
			t.Errorf("%v accepted", env)
		}
		if _, err := os.Stat(filepath.Join(e.stub, "ssh.argv")); err == nil {
			t.Errorf("%v: ssh called", env)
		}
	}
}

// install.sh: the workstation service gets the same settings, the files go in
// the checkout root.
func TestInstallPersonalKeysUnsetIsToday(t *testing.T) {
	e, _, env := installEnv(t)
	env = append(env, "VMTOOL_DOMAIN=example.lab")
	keysBao(t, e, pullTok+"\n")
	r := runScript(t, e.stub, e.wd, "install.sh", append(env, "BAO_KEYS_PULL_TOKEN_PATH="+keysPath))
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	want := "config write --name fallbackhost --role workstation --listen 127.0.0.1:9473 " +
		"--tls-cert tls/fallbackhost.example.lab.crt --tls-key tls/fallbackhost.example.lab.key"
	if got := strings.TrimSpace(readOr(t, filepath.Join(e.wd, "vmtool.argv"))); got != want {
		t.Errorf("config write argv:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "bao.argv")), "-field=token") {
		t.Error("bao asked for a pull token")
	}
	if _, err := os.Stat(filepath.Join(e.wd, "keys-pull-token")); err == nil {
		t.Error("keys-pull-token written")
	}
}

func TestInstallPersonalKeysSet(t *testing.T) {
	e, _, env := installEnv(t)
	env = append(env, "VMTOOL_DOMAIN=example.lab")
	keysBao(t, e, pullTok+"\n")
	r := runScript(t, e.stub, e.wd, "install.sh", append(env, append([]string{"VMTOOL_KEY_REQUEST_URL=" + keysReqURL}, keysSet...)...))
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	want := "config write --name fallbackhost --role workstation --listen 127.0.0.1:9473 " +
		"--tls-cert tls/fallbackhost.example.lab.crt --tls-key tls/fallbackhost.example.lab.key " +
		"--personal-keys-url " + keysListURL + " --personal-keys-token-file keys-pull-token " +
		"--personal-keys-cache-file personal-keys.json --key-request-url " + keysReqURL
	if got := strings.TrimSpace(readOr(t, filepath.Join(e.wd, "vmtool.argv"))); got != want {
		t.Errorf("config write argv:\n got %q\nwant %q", got, want)
	}
	f := filepath.Join(e.wd, "keys-pull-token")
	if got := readOr(t, f); strings.TrimSpace(got) != pullTok {
		t.Errorf("keys-pull-token has the wrong content (len %d)", len(got))
	}
	if m := fileMode(t, f); m != 0o600 {
		t.Errorf("keys-pull-token mode %v", m)
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "bao.argv")), "kv get -field=token "+keysPath+"\n") {
		t.Errorf("bao argv: %q", readOr(t, filepath.Join(e.stub, "bao.argv")))
	}
	if strings.Contains(r.stdout+r.stderr, pullTok) {
		t.Error("pull token printed")
	}
	for _, f := range []string{"sudo.argv", "systemctl.argv", "go.argv", "vmtool.argv"} {
		for _, p := range []string{filepath.Join(e.stub, f), filepath.Join(e.wd, f)} {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), pullTok) {
				t.Errorf("pull token in %s", p)
			}
		}
	}
}

// A missing pull token with the URL set stops install before the build, the
// unit and the restart; an existing token file is left alone.
func TestInstallPersonalKeysMissingPullTokenIsFatal(t *testing.T) {
	for _, tc := range []struct {
		name, tok string
		env       []string
		want      string
	}{
		{"absent", "", keysSet, "no pull token"},
		{"empty", "\n", keysSet, "no pull token"},
		{"path unset", pullTok, []string{"VMTOOL_PERSONAL_KEYS_URL=" + keysListURL}, "BAO_KEYS_PULL_TOKEN_PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, unit, env := installEnv(t)
			env = append(env, "VMTOOL_DOMAIN=example.lab")
			keysBao(t, e, tc.tok)
			r := runScript(t, e.stub, e.wd, "install.sh", append(env, tc.env...))
			if r.code == 0 || !strings.Contains(r.stderr, tc.want) {
				t.Fatalf("exit %d stderr %q, want a failure naming %q", r.code, r.stderr, tc.want)
			}
			for _, f := range []string{"sudo.argv", "systemctl.argv"} {
				if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
					t.Errorf("%s exists: install went on", f)
				}
			}
			if _, err := os.Stat(unit); err == nil {
				t.Error("the unit was installed")
			}
			if _, err := os.Stat(filepath.Join(e.wd, "keys-pull-token")); err == nil {
				t.Error("keys-pull-token written")
			}
		})
	}
}

func TestInstallEnvFile(t *testing.T) {
	e, _, env := installEnv(t)
	env = append(env, "VMTOOL_DOMAIN=example.lab")
	keysBao(t, e, pullTok+"\n")
	if err := os.WriteFile(filepath.Join(e.wd, "deploy.env"), []byte("VMTOOL_PERSONAL_KEYS_URL="+keysListURL+"\nBAO_KEYS_PULL_TOKEN_PATH="+keysPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runScript(t, e.stub, e.wd, "install.sh", env)
	if r.code != 0 || !strings.Contains(readOr(t, filepath.Join(e.wd, "vmtool.argv")), "--personal-keys-url "+keysListURL) {
		t.Fatalf("exit %d: %s%s\nargv %q", r.code, r.stdout, r.stderr, readOr(t, filepath.Join(e.wd, "vmtool.argv")))
	}
	// A bad line stops before the build.
	e, _, env = installEnv(t)
	env = append(env, "VMTOOL_DOMAIN=example.lab")
	if err := os.WriteFile(filepath.Join(e.wd, "deploy.env"), []byte("\nVMTOOL_TOKEN=SECRETSECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runScript(t, e.stub, e.wd, "install.sh", env)
	if r.code == 0 || !strings.Contains(r.stderr, "deploy.env line 2") || strings.Contains(r.stderr, "SECRETSECRET") {
		t.Errorf("exit %d stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.stub, "go.argv")); err == nil {
		t.Error("built after a bad deploy.env")
	}
}

func TestPersonalKeysIgnoredFiles(t *testing.T) {
	b, err := os.ReadFile("../.gitignore")
	if err != nil {
		t.Fatal(err)
	}
	lines := "\n" + string(b)
	for _, f := range []string{"/deploy.env", "/keys-pull-token", "/personal-keys.json"} {
		if !strings.Contains(lines, "\n"+f+"\n") {
			t.Errorf(".gitignore lacks %s", f)
		}
	}
}
