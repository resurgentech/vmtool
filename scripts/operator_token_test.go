package scripts_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture operator tokens (random hex, not real).
const (
	opA = "3f9a1c07b2d84e65a0c1f7e93b8d2a46c5e0917f8a3b6d2c4e1f09a7b5c83d21"
	opB = "7c21e9d05a4b83f6e1a09c7d2b5f4830a6d9e1c8f27b3a5409d6e8c1b7a2f354"
	opC = "d4086b1fa93e7c25b0d8a6f1e39c47520b8e1d6a4f93c7e2158b0a6d3f9e41c7"

	opBaoErr = "OPBAODECOYERRORTEXT"
)

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// opBao makes the stub bao serve the test CA and the operator token at the
// default test path. optok is the stored value: "" leaves the path absent
// (bao fails with a decoy error on stderr that must never reach the output).
func opBao(t *testing.T, e *certEnv, optok string) {
	t.Helper()
	addStub(t, e.stub, "bao", `if [[ "$1 $2 $3" == "kv get -field=string" ]]; then
  case "$4" in
    */crt) cat '`+e.caCrt+`' ;;
    */key) cat '`+e.caKey+`' ;;
    `+fakePath+`)
      if [[ -f $d/optok ]]; then cat "$d/optok"; exit 0; fi
      echo "`+opBaoErr+` no value found" >&2; exit 2 ;;
    *) exit 3 ;;
  esac
fi`)
	if optok != "" {
		writeStub(t, e, "optok", optok)
	}
}

type opSource int

const (
	srcEnv opSource = iota
	srcDotEnv
	srcBao
)

var opSourceLine = map[opSource]string{
	srcEnv:    "environment",
	srcDotEnv: ".env",
	srcBao:    "OpenBao (BAO_TOKEN_PATH)",
}

// opCase is one way to hand a run its operator token: the environment, .env
// and OpenBao each hold the value given (empty: none).
type opCase struct {
	name             string
	env, dotenv, bao string
	want             string
	src              opSource
}

var opCases = []opCase{
	{"environment wins", opA, opB, opC, opA, srcEnv},
	{".env wins over OpenBao", "", opB, opC, opB, srcDotEnv},
	{"OpenBao when nothing else", "", "", opC, opC, srcBao},
}

func opDotEnv(tok string) string {
	if tok == "" {
		return ""
	}
	return "KEEP=1\nVMTOOL_TOKEN=" + tok + "\n"
}

// opLeaks fails if tok is in the run's output, in any recorded argv, or in
// any file under the stub, work and extra dirs except the places it may be:
// bao's own answer file, the stdin of the config write call, vmtool.stdin and
// (for the .env source) .env itself. What the stubs' environments held is
// checked too, unless the token is in the environment on purpose.
func opLeaks(t *testing.T, r result, tok string, src opSource, dump string, e *certEnv, extra ...string) {
	t.Helper()
	if strings.Contains(r.stdout+r.stderr, tok) {
		t.Errorf("the token is in the output: %s%s", r.stdout, r.stderr)
	}
	for _, d := range append([]string{e.stub, e.wd}, extra...) {
		filepath.Walk(d, func(p string, fi os.FileInfo, err error) error {
			if err != nil || !fi.Mode().IsRegular() {
				return nil
			}
			b := filepath.Base(p)
			switch {
			case b == "optok", b == "vmtool.stdin":
				return nil
			case b == ".env" && filepath.Dir(p) == e.wd && src == srcDotEnv:
				return nil
			case strings.HasPrefix(b, "ssh.") && strings.HasSuffix(b, ".stdin"):
				a := readOr(t, strings.TrimSuffix(p, ".stdin")+".argv")
				if strings.Contains(a, "config write") {
					return nil
				}
			}
			if d, _ := os.ReadFile(p); strings.Contains(string(d), tok) {
				t.Errorf("the token is in %s", p)
			}
			return nil
		})
	}
	if src == srcEnv {
		return
	}
	ents, _ := os.ReadDir(dump)
	if len(ents) == 0 {
		t.Error("no stub recorded its environment")
	}
	for _, en := range ents {
		d := readOr(t, filepath.Join(dump, en.Name()))
		if strings.Contains(d, tok) || strings.Contains(d, "\nVMTOOL_TOKEN=") || strings.HasPrefix(d, "VMTOOL_TOKEN=") {
			t.Errorf("a stub's environment (%s) holds the token or a VMTOOL_TOKEN variable", en.Name())
		}
	}
}

func opNoSideEffects(t *testing.T, e *certEnv, unit string) {
	t.Helper()
	for _, f := range []string{"ssh.argv", "scp.argv", "sudo.argv", "systemctl.argv", "go.argv", "tar.stdin", "installed.list"} {
		if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
			t.Errorf("%s exists: the run went on without a token", f)
		}
	}
	for _, f := range []string{"vmtool.argv", "tls", "keys-pull-token"} {
		if _, err := os.Stat(filepath.Join(e.wd, f)); err == nil {
			t.Errorf("%s exists in the checkout: the run went on without a token", f)
		}
	}
	if unit != "" {
		if _, err := os.Stat(unit); err == nil {
			t.Error("the unit was installed")
		}
	}
	// bao may have been asked for the token, never for the CA.
	argv := readOr(t, filepath.Join(e.stub, "bao.argv"))
	if strings.Contains(argv, "/crt") || strings.Contains(argv, "/key") {
		t.Errorf("the CA was read before the token check: %q", argv)
	}
}

func readBytes(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return b
}

func sourceLines(out string) []string {
	var ls []string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "operator token: ") {
			ls = append(ls, l)
		}
	}
	return ls
}

// opDeployEnv is the remote deploy environment with a token nowhere but
// where the case puts it, an environment dump and the operator-token bao.
func opDeployEnv(t *testing.T, c opCase) (e *certEnv, rhome, hv, dump string) {
	t.Helper()
	e, rhome, hv = newRemoteEnv(t)
	e.withoutToken()
	opBao(t, e, c.bao)
	dump = t.TempDir()
	e.env = append(e.env, "VMTOOL_DOMAIN=example.lab", "VMTOOL_TEST_ENVDUMP="+dump)
	if c.env != "" {
		e.env = append(e.env, "VMTOOL_TOKEN="+c.env)
	}
	if c.dotenv != "" {
		if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte(opDotEnv(c.dotenv)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func checkDeployToken(t *testing.T, e *certEnv, r result, c opCase, dump string, rhome, hv string) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if ls := sourceLines(r.stdout); len(ls) != 1 || ls[0] != "operator token: "+opSourceLine[c.src] {
		t.Errorf("source lines %q, want one naming %q", ls, opSourceLine[c.src])
	}
	if got := configWriteCall(t, e).stdin; got != c.want+"\n" {
		t.Errorf("config write stdin is not the %s token (len %d)", opSourceLine[c.src], len(got))
	}
	if got := readOr(t, filepath.Join(e.stub, "installed", "config.yml")); !strings.Contains(got, sha(c.want)) {
		t.Errorf("installed config.yml has the wrong token_sha256: %q", got)
	}
	argv := readOr(t, filepath.Join(e.stub, "bao.argv"))
	if reads := strings.Count(argv, "kv get -field=string "+fakePath+"\n"); (c.src == srcBao) != (reads == 1) || reads > 1 {
		t.Errorf("bao read the token %d times for source %s", reads, opSourceLine[c.src])
	}
	opLeaks(t, r, c.want, c.src, dump, e, rhome, hv)
	for _, other := range []string{opA, opB, opC} {
		if other != c.want {
			for _, call := range sshCalls(t, e.stub) {
				if strings.Contains(call.stdin, other) || strings.Contains(call.argv, other) {
					t.Errorf("an ssh call carries a token that did not win")
				}
			}
		}
	}
}

// Each source wins in turn: the environment, then .env, then OpenBao; one
// line names the source and the token goes on stdin of the config write only.
func TestDeployOperatorTokenSources(t *testing.T) {
	for _, c := range opCases {
		t.Run(c.name, func(t *testing.T) {
			e, rhome, hv, dump := opDeployEnv(t, c)
			before := readBytes(t, filepath.Join(e.wd, ".env"))
			r := e.deploy(t, nil, "hv1")
			checkDeployToken(t, e, r, c, dump, rhome, hv)
			if after := readBytes(t, filepath.Join(e.wd, ".env")); string(before) != string(after) {
				t.Error(".env changed")
			}
		})
	}
}

// With the session only in .env (the two export lines) and nothing in the
// environment, a deploy through just reads the token from OpenBao.
func TestDeployOperatorTokenFromOpenBaoThroughJust(t *testing.T) {
	c := opCase{bao: opC, want: opC, src: srcBao}
	e, rhome, hv, dump := opDeployEnv(t, c)
	e.env = noSession(e.env)
	body := "export VAULT_ADDR=" + fixAddr + "\nexport VAULT_TOKEN=" + fixTok + "\n"
	if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := justReal(t, e, append(e.env, "BAO_TOKEN_PATH="+fakePath), "deploy", "hv1")
	checkDeployToken(t, e, r, c, dump, rhome, hv)
	if after := readBytes(t, filepath.Join(e.wd, ".env")); string(after) != body {
		t.Error(".env changed")
	}
}

// The same token gives the same token_sha256 from every source.
func TestOperatorTokenSameHashFromEverySource(t *testing.T) {
	for _, c := range []opCase{
		{"env", opA, "", "", opA, srcEnv},
		{"dotenv", "", opA, "", opA, srcDotEnv},
		{"bao", "", "", opA, opA, srcBao},
	} {
		t.Run("deploy "+c.name, func(t *testing.T) {
			e, rhome, hv, dump := opDeployEnv(t, c)
			r := e.deploy(t, nil, "hv1")
			checkDeployToken(t, e, r, c, dump, rhome, hv)
			if got := readOr(t, filepath.Join(e.stub, "installed", "config.yml")); !strings.Contains(got, "token_sha256: "+sha(opA)) {
				t.Errorf("installed config.yml: %q", got)
			}
		})
		t.Run("install "+c.name, func(t *testing.T) {
			e, unit, env, dump := opInstallEnv(t, c)
			r := runScript(t, e.stub, e.wd, "install.sh", env)
			checkInstallToken(t, e, unit, r, c, dump)
		})
	}
}

// None of the sources has a token: stop by name before anything is copied,
// built, certified, installed or restarted.
var opNone = []struct {
	name, bao string
	env       []string
	want      []string
}{
	{"path unset", opC, []string{"BAO_TOKEN_PATH="}, []string{"BAO_TOKEN_PATH is not set"}},
	{"openbao has none", "", nil, []string{"BAO_TOKEN_PATH", fakePath}},
	{"openbao empty", "\n", nil, []string{"BAO_TOKEN_PATH", fakePath}},
	{"openbao blank", " \t \n", nil, []string{"BAO_TOKEN_PATH", fakePath}},
}

func checkNoTokenMessage(t *testing.T, r result, want []string) {
	t.Helper()
	if r.code == 0 {
		t.Fatalf("exit 0: %s%s", r.stdout, r.stderr)
	}
	for _, w := range append([]string{"no operator token", "VMTOOL_TOKEN", ".env"}, want...) {
		if !strings.Contains(r.stderr, w) {
			t.Errorf("stderr lacks %q: %q", w, r.stderr)
		}
	}
	// A token in .env overrides every client's own key file, so no message
	// suggests putting one there.
	for _, bad := range []string{"export", "token-fetch", "VMTOOL_TOKEN in .env", opBaoErr} {
		if strings.Contains(r.stdout+r.stderr, bad) {
			t.Errorf("the output holds %q: %s%s", bad, r.stdout, r.stderr)
		}
	}
	if len(sourceLines(r.stdout)) != 0 {
		t.Errorf("a source line was printed with no token: %q", r.stdout)
	}
}

func TestDeployNoOperatorToken(t *testing.T) {
	for _, c := range opNone {
		t.Run(c.name, func(t *testing.T) {
			e, _, _, _ := opDeployEnv(t, opCase{})
			opBao(t, e, c.bao)
			if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte("KEEP=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			r := e.deploy(t, c.env, "hv1")
			checkNoTokenMessage(t, r, c.want)
			opNoSideEffects(t, e, "")
			if got := readOr(t, filepath.Join(e.wd, ".env")); got != "KEEP=1\n" {
				t.Errorf(".env changed: %q", got)
			}
		})
	}
}

func opInstallEnv(t *testing.T, c opCase) (e *certEnv, unit string, env []string, dump string) {
	t.Helper()
	e, unit, env = installEnv(t)
	opBao(t, e, c.bao)
	dump = t.TempDir()
	env = append(env, "VMTOOL_DOMAIN=example.lab", "VMTOOL_TEST_ENVDUMP="+dump)
	if c.env != "" {
		env = append(env, "VMTOOL_TOKEN="+c.env)
	}
	if err := os.Remove(filepath.Join(e.wd, ".env")); err != nil {
		t.Fatal(err)
	}
	if c.dotenv != "" {
		if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte(opDotEnv(c.dotenv)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return
}

func checkInstallToken(t *testing.T, e *certEnv, unit string, r result, c opCase, dump string) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if ls := sourceLines(r.stdout); len(ls) != 1 || ls[0] != "operator token: "+opSourceLine[c.src] {
		t.Errorf("source lines %q, want one naming %q", ls, opSourceLine[c.src])
	}
	if got := readOr(t, filepath.Join(e.wd, "vmtool.stdin")); got != c.want+"\n" {
		t.Errorf("config write stdin is not the %s token (len %d)", opSourceLine[c.src], len(got))
	}
	argv := readOr(t, filepath.Join(e.wd, "vmtool.argv"))
	if !strings.HasPrefix(argv, "config write --token-stdin ") {
		t.Errorf("config write argv %q", argv)
	}
	bao := readOr(t, filepath.Join(e.stub, "bao.argv"))
	if reads := strings.Count(bao, "kv get -field=string "+fakePath+"\n"); (c.src == srcBao) != (reads == 1) || reads > 1 {
		t.Errorf("bao read the token %d times for source %s", reads, opSourceLine[c.src])
	}
	opLeaks(t, r, c.want, c.src, dump, e)
	if c.src == srcEnv {
		// The environment already had it; the vmtool child has exactly that.
		ents, _ := os.ReadDir(dump)
		for _, en := range ents {
			if strings.HasPrefix(en.Name(), "vmtool.") {
				if d := readOr(t, filepath.Join(dump, en.Name())); !strings.Contains(d, "VMTOOL_TOKEN="+c.want+"\n") {
					t.Errorf("the vmtool child lost the VMTOOL_TOKEN it had")
				}
			}
		}
	}
	got := false
	ents, _ := os.ReadDir(dump)
	for _, en := range ents {
		got = got || strings.HasPrefix(en.Name(), "vmtool.")
	}
	if !got {
		t.Error("the vmtool stub recorded no environment")
	}
	if _, err := os.Stat(unit); err != nil {
		t.Errorf("no unit installed: %v", err)
	}
}

// install: the same sources, the same order; the token reaches `config write`
// on stdin only, so vmtool's environment holds no VMTOOL_TOKEN it did not
// already have.
func TestInstallOperatorTokenSources(t *testing.T) {
	for _, c := range opCases {
		t.Run(c.name, func(t *testing.T) {
			e, unit, env, dump := opInstallEnv(t, c)
			before := readBytes(t, filepath.Join(e.wd, ".env"))
			r := runScript(t, e.stub, e.wd, "install.sh", env)
			checkInstallToken(t, e, unit, r, c, dump)
			if after := readBytes(t, filepath.Join(e.wd, ".env")); string(before) != string(after) {
				t.Error(".env changed")
			}
		})
	}
}

func TestInstallOperatorTokenFromOpenBaoThroughJust(t *testing.T) {
	c := opCase{bao: opC, want: opC, src: srcBao}
	e, unit, env, dump := opInstallEnv(t, c)
	body := "export VAULT_ADDR=" + fixAddr + "\nexport VAULT_TOKEN=" + fixTok + "\n"
	if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := justReal(t, e, append(noSession(env), "BAO_TOKEN_PATH="+fakePath), "install")
	checkInstallToken(t, e, unit, r, c, dump)
	if after := readBytes(t, filepath.Join(e.wd, ".env")); string(after) != body {
		t.Error(".env changed")
	}
}

func TestInstallNoOperatorToken(t *testing.T) {
	for _, c := range opNone {
		t.Run(c.name, func(t *testing.T) {
			e, unit, env, _ := opInstallEnv(t, opCase{})
			opBao(t, e, c.bao)
			if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte("KEEP=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			r := runScript(t, e.stub, e.wd, "install.sh", append(env, c.env...))
			checkNoTokenMessage(t, r, c.want)
			opNoSideEffects(t, e, unit)
			if got := readOr(t, filepath.Join(e.wd, ".env")); got != "KEEP=1\n" {
				t.Errorf(".env changed: %q", got)
			}
		})
	}
}

// No message of the scripts sends an operator to `just token-fetch` for the
// deploy or install token, or to export it.
func TestOperatorTokenMessagesNameTheSettings(t *testing.T) {
	for _, f := range []string{"deploy.sh", "install.sh", "deploy-env.sh"} {
		for i, l := range strings.Split(readText(t, f), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue // comments may say what token-fetch is for
			}
			if strings.Contains(l, "token-fetch") || strings.Contains(l, "export the operator token") {
				t.Errorf("%s:%d still points at token-fetch or export: %s", f, i+1, l)
			}
		}
	}
}

// just settings says what BAO_TOKEN_PATH is for now.
func TestSettingsTokenPathNeeds(t *testing.T) {
	r := runScript(t, t.TempDir(), t.TempDir(), "settings.sh", blankEnv())
	l := settingLine(t, r.stdout, "BAO_TOKEN_PATH")
	for _, w := range []string{"unset", "token-fetch", "deploy", "install"} {
		if !strings.Contains(l, w) {
			t.Errorf("BAO_TOKEN_PATH row lacks %q: %q", w, l)
		}
	}
}
