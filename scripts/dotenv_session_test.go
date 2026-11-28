package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The recipes whose scripts call bao source the checkout's .env in the shell
// line that runs the script; no other recipe does.
var dotenvRecipes = []string{"cert", "deploy", "install", "token", "token-fetch"}

const (
	fixAddr = "http://127.0.0.1:1/FIXADDR"
	fixTok  = "fixvaulttokenfixvaulttokenfixvaulttoken"
)

func needJust(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("just")
	if err != nil {
		t.Skip("just not installed")
	}
	return p
}

// justRecipes returns every recipe of the real justfile with the arguments a
// dry run needs (one dummy per parameter).
func justRecipes(t *testing.T) map[string][]string {
	t.Helper()
	out, err := exec.Command(needJust(t), "--justfile", "../justfile", "--dump", "--dump-format", "json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Recipes map[string]struct {
			Parameters []struct{ Name string }
		}
	}
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatal(err)
	}
	res := map[string][]string{}
	for n, r := range d.Recipes {
		args := []string{}
		for range r.Parameters {
			args = append(args, "x")
		}
		res[n] = args
	}
	return res
}

var dotenvPrefixRe = regexp.MustCompile(`if \[ -f "[^"]*/\.env" \]; then set -a; \. "[^"]*/\.env"; set \+a; fi;`)

// Exactly the listed recipes carry the one shared prefix; nothing else in the
// justfile touches .env.
func TestJustDotEnvOnlyInListedRecipes(t *testing.T) {
	just := needJust(t)
	listed := map[string]bool{}
	for _, r := range dotenvRecipes {
		listed[r] = true
	}
	recipes := justRecipes(t)
	names := make([]string, 0, len(recipes))
	for n := range recipes {
		names = append(names, n)
	}
	sort.Strings(names)
	var prefixes []string
	for _, n := range names {
		out, err := exec.Command(just, append([]string{"--justfile", "../justfile", "--dry-run", n}, recipes[n]...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", n, err, out)
		}
		has := strings.Contains(string(out), "/.env")
		if listed[n] != has {
			t.Errorf("recipe %s: sources .env = %v, want %v:\n%s", n, has, listed[n], out)
		}
		if listed[n] {
			p := dotenvPrefixRe.FindString(string(out))
			if p == "" {
				t.Errorf("recipe %s lacks the shared prefix:\n%s", n, out)
			}
			prefixes = append(prefixes, p)
		}
	}
	for _, p := range prefixes {
		if p != prefixes[0] {
			t.Errorf("the prefixes differ: %q vs %q", p, prefixes[0])
		}
	}
	for _, l := range dotenvRecipes {
		if _, ok := recipes[l]; !ok {
			t.Errorf("no recipe %s", l)
		}
	}
}

const recStub = `#!/bin/sh
printf '%s ADDR=%s TOKEN=%s WANT=%s\n' "$(basename "$0")" "${VAULT_ADDR:+set}" "${VAULT_TOKEN:+set}" \
  "$([ "$VAULT_ADDR" = "$VMTOOL_TEST_WANT_ADDR" ] && echo yes || echo no)" >> "$VMTOOL_TEST_REC"
`

// stubScriptsRoot is a scratch checkout: the real justfile, and a scripts dir
// of recording stand-ins, plus a stub go.
func stubScriptsRoot(t *testing.T) (root, rec, bin string) {
	t.Helper()
	root = t.TempDir()
	jf, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "justfile"), jf, 0o600); err != nil {
		t.Fatal(err)
	}
	sc := t.TempDir()
	for _, n := range []string{"token.sh", "cert.sh", "install.sh", "deploy.sh", "test-capped.sh"} {
		if err := os.WriteFile(filepath.Join(sc, n), []byte(recStub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(sc, filepath.Join(root, "scripts")); err != nil {
		t.Fatal(err)
	}
	bin = t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(recStub), 0o755); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(t.TempDir(), "rec"), bin
}

// runJust runs `just <args>` in a scratch root with a scrubbed environment
// plus env (never the caller's VAULT_*).
func runJust(t *testing.T, bin, root string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(needJust(t), append([]string{"--justfile", filepath.Join(root, "justfile")}, args...)...)
	cmd.Dir = root
	cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + root}, env...)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{so.String(), se.String(), code}
}

func recipeArgs(name string) []string {
	if name == "cert" || name == "deploy" {
		return []string{name, "h1"}
	}
	return []string{name}
}

// The session in .env reaches the script of each listed recipe, in the
// `export` and the plain form, with nothing exported; the recipes that do not
// call bao never see it.
func TestJustDotEnvReachesListedRecipesOnly(t *testing.T) {
	for _, form := range []struct{ name, body string }{
		{"export", "export VAULT_ADDR=" + fixAddr + "\nexport VAULT_TOKEN=" + fixTok + "\n"},
		{"plain", "VAULT_ADDR=" + fixAddr + "\nVAULT_TOKEN=" + fixTok + "\n"},
	} {
		t.Run(form.name, func(t *testing.T) {
			root, rec, bin := stubScriptsRoot(t)
			if err := os.WriteFile(filepath.Join(root, ".env"), []byte(form.body), 0o600); err != nil {
				t.Fatal(err)
			}
			env := []string{"VMTOOL_TEST_REC=" + rec, "VMTOOL_TEST_WANT_ADDR=" + fixAddr}
			for _, r := range append(append([]string{}, dotenvRecipes...), "test", "build") {
				if out := runJust(t, bin, root, env, recipeArgs(r)...); out.code != 0 {
					t.Fatalf("just %s: %d %s%s", r, out.code, out.stdout, out.stderr)
				}
			}
			got := map[string]string{}
			for _, l := range strings.Split(strings.TrimSpace(readOr(t, rec)), "\n") {
				f := strings.Fields(l)
				got[f[0]] += strings.Join(f[1:], " ") + ";"
			}
			for _, s := range []string{"token.sh", "cert.sh", "install.sh", "deploy.sh"} {
				if want := "ADDR=set TOKEN=set WANT=yes;"; !strings.Contains(got[s], want) {
					t.Errorf("%s saw %q, want %q", s, got[s], want)
				}
			}
			// token.sh runs for two recipes (token, token-fetch).
			if got["token.sh"] != strings.Repeat("ADDR=set TOKEN=set WANT=yes;", 2) {
				t.Errorf("token.sh runs: %q", got["token.sh"])
			}
			for _, s := range []string{"test-capped.sh", "go"} {
				if got[s] != "ADDR= TOKEN= WANT=no;" {
					t.Errorf("%s (not on the list) saw %q, want no session", s, got[s])
				}
			}
		})
	}
}

// With no .env the recipes run as they do today; a .env value beats the
// calling shell's for the listed recipes only.
func TestJustDotEnvAbsentAndOverride(t *testing.T) {
	root, rec, bin := stubScriptsRoot(t)
	env := []string{"VMTOOL_TEST_REC=" + rec, "VMTOOL_TEST_WANT_ADDR=" + fixAddr}
	if out := runJust(t, bin, root, env, "token-fetch"); out.code != 0 {
		t.Fatalf("no .env: %d %s%s", out.code, out.stdout, out.stderr)
	}
	if got := strings.TrimSpace(readOr(t, rec)); got != "token.sh ADDR= TOKEN= WANT=no" {
		t.Errorf("no .env: %q", got)
	}
	// The shell holds another address and a token; .env has only the address.
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("VAULT_ADDR="+fixAddr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	shell := append(env, "VAULT_ADDR=http://127.0.0.1:2/shell", "VAULT_TOKEN="+fixTok)
	if out := runJust(t, bin, root, shell, "token-fetch"); out.code != 0 {
		t.Fatalf("%d %s%s", out.code, out.stdout, out.stderr)
	}
	if got := strings.TrimSpace(readOr(t, rec)); got != "token.sh ADDR=set TOKEN=set WANT=yes" {
		t.Errorf("the .env address must win and the shell's token stay: %q", got)
	}
	// An unlisted recipe keeps the shell's value.
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	if out := runJust(t, bin, root, shell, "test"); out.code != 0 {
		t.Fatalf("%d %s%s", out.code, out.stdout, out.stderr)
	}
	if got := strings.TrimSpace(readOr(t, rec)); got != "test-capped.sh ADDR=set TOKEN=set WANT=no" {
		t.Errorf("test: %q", got)
	}
}

// recBao is a bao that serves the test CA and records, per call, whether the
// session is in its environment (never the values), and dumps nothing else.
func recBao(t *testing.T, e *certEnv) {
	t.Helper()
	addStub(t, e.stub, "bao", `echo "ADDR=${VAULT_ADDR:+set} TOKEN=${VAULT_TOKEN:+set}" >> "$d/bao.session"
if [[ "$1 $2 $3" == "kv get -field=string" ]]; then
  case "$4" in
    */crt) cat '`+e.caCrt+`' ;;
    */key) cat '`+e.caKey+`' ;;
    *) exit 3 ;;
  esac
fi`)
}

// justReal runs the real scripts of a recipe through just in the checkout
// held by e.wd (the real justfile, the real scripts dir and deploy dir
// symlinked in), with e.stub first on PATH.
func justReal(t *testing.T, e *certEnv, env []string, args ...string) result {
	t.Helper()
	jf, err := os.ReadFile("../justfile")
	if err != nil {
		t.Fatal(err)
	}
	// The justfile pins the daemon's directories; a test must never reach the
	// real ones, so the scratch copy points them at throwaway directories.
	hv, pool := e.get("HV_DIR"), t.TempDir()
	if hv == "" {
		hv = t.TempDir()
	}
	text := strings.NewReplacer(`hv_dir := "/opt/vmtool"`, `hv_dir := "`+hv+`"`,
		`hv_pool_dir := "/var/lib/vmtool/images"`, `hv_pool_dir := "`+pool+`"`).Replace(string(jf))
	if strings.Contains(text, `"/opt/vmtool"`) || strings.Contains(text, `"/var/lib/vmtool`) {
		t.Fatal("the justfile still names the real daemon directories")
	}
	if err := os.WriteFile(filepath.Join(e.wd, "justfile"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"scripts", "deploy"} {
		abs, err := filepath.Abs(filepath.Join("..", d))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(abs, filepath.Join(e.wd, d)); err != nil && !os.IsExist(err) {
			t.Fatal(err)
		}
	}
	return runJust(t, e.stub, e.wd, env, args...)
}

func sessionRecords(t *testing.T, e *certEnv) []string {
	t.Helper()
	s := strings.TrimSpace(readOr(t, filepath.Join(e.stub, "bao.session")))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func noValueAnywhere(t *testing.T, r result, dirs ...string) {
	t.Helper()
	for _, v := range []string{fixAddr, fixTok, "FIXADDR"} {
		if strings.Contains(r.stdout+r.stderr, v) {
			t.Errorf("a session value is in the output: %s%s", r.stdout, r.stderr)
		}
		for _, d := range dirs {
			filepath.Walk(d, func(p string, fi os.FileInfo, err error) error {
				if err != nil || !fi.Mode().IsRegular() || filepath.Base(p) == ".env" {
					return nil
				}
				if b, _ := os.ReadFile(p); strings.Contains(string(b), v) {
					t.Errorf("a session value is in %s", p)
				}
				return nil
			})
		}
	}
}

// token-fetch, through just, finds the session in .env with nothing exported.
func TestJustTokenFetchFindsSessionInDotEnv(t *testing.T) {
	for _, form := range []string{"export ", ""} {
		stub := stubBin(t, "bao", `echo "ADDR=${VAULT_ADDR:+set} TOKEN=${VAULT_TOKEN:+set}" >> "$d/bao.session"
if [[ "$1 $2" == "kv get" ]]; then echo `+deployTok+`; fi`)
		e := &certEnv{stub: stub, wd: t.TempDir()}
		if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte(form+"VAULT_ADDR="+fixAddr+"\n"+form+"VAULT_TOKEN="+fixTok+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := justReal(t, e, []string{"BAO_TOKEN_PATH=" + fakePath}, "token-fetch")
		if r.code != 0 {
			t.Fatalf("form %q: exit %d: %s%s", form, r.code, r.stdout, r.stderr)
		}
		if got := sessionRecords(t, e); len(got) != 1 || got[0] != "ADDR=set TOKEN=set" {
			t.Errorf("form %q: bao saw %q", form, got)
		}
		noValueAnywhere(t, r, stub)
	}
}

// cert, through just, with the session only in .env.
func TestJustCertFindsSessionInDotEnv(t *testing.T) {
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	recBao(t, e)
	if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte("export VAULT_ADDR="+fixAddr+"\nVAULT_TOKEN="+fixTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := justReal(t, e, noSession(e.env), "cert", "jetfire")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	got := sessionRecords(t, e)
	if len(got) == 0 {
		t.Fatal("bao was not called")
	}
	for _, g := range got {
		if g != "ADDR=set TOKEN=set" {
			t.Errorf("bao saw %q", g)
		}
	}
	noValueAnywhere(t, r, e.stub)
}

// install, through just, with the session only in .env (the CA read and the
// pull token read).
func TestJustInstallFindsSessionInDotEnv(t *testing.T) {
	e, unit, env := installEnv(t)
	keysBao(t, e, pullTok+"\n")
	addStub(t, e.stub, "bao", `echo "ADDR=${VAULT_ADDR:+set} TOKEN=${VAULT_TOKEN:+set}" >> "$d/bao.session"
if [[ "$1 $2" == "kv get" ]]; then
  case "$3 $4" in
    "-field=string "*/crt) cat '`+e.caCrt+`' ;;
    "-field=string "*/key) cat '`+e.caKey+`' ;;
    "-field=token `+keysPath+`") cat "$d/pulltok" ;;
    *) exit 3 ;;
  esac
fi`)
	if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte("VMTOOL_TOKEN=abc\nexport VAULT_ADDR="+fixAddr+"\nexport VAULT_TOKEN="+fixTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env = append(env, "VMTOOL_DOMAIN=example.lab")
	r := justReal(t, e, append(noSession(env), keysSet...), "install")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	got := sessionRecords(t, e)
	if len(got) < 3 { // two for the CA, one for the pull token
		t.Fatalf("bao calls: %q", got)
	}
	for _, g := range got {
		if g != "ADDR=set TOKEN=set" {
			t.Errorf("bao saw %q", g)
		}
	}
	_ = unit
	noValueAnywhere(t, r, e.stub)
}

// deploy, through just: the CA read and the pull token read see the session
// from .env; no ssh call has it in its environment or on its command line.
func TestJustDeployFindsSessionAndKeepsItOffSSH(t *testing.T) {
	e, _, _ := keysEnv(t, pullTok+"\n")
	addStub(t, e.stub, "bao", `echo "ADDR=${VAULT_ADDR:+set} TOKEN=${VAULT_TOKEN:+set}" >> "$d/bao.session"
if [[ "$1 $2" == "kv get" ]]; then
  case "$3 $4" in
    "-field=string "*/crt) cat '`+e.caCrt+`' ;;
    "-field=string "*/key) cat '`+e.caKey+`' ;;
    "-field=token `+keysPath+`") cat "$d/pulltok" ;;
    *) exit 3 ;;
  esac
fi`)
	if err := os.WriteFile(filepath.Join(e.wd, ".env"), []byte("export VAULT_ADDR="+fixAddr+"\nexport VAULT_TOKEN="+fixTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := justReal(t, e, append(noSession(e.env), keysSet...), "deploy", "hv1")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	for _, g := range sessionRecords(t, e) {
		if g != "ADDR=set TOKEN=set" {
			t.Errorf("bao saw %q", g)
		}
	}
	calls := sshCalls(t, e.stub)
	if len(calls) == 0 {
		t.Fatal("no ssh calls")
	}
	for i := range calls {
		if v := readOr(t, filepath.Join(e.stub, "ssh."+strconv.Itoa(i+1)+".vault")); v != "" {
			t.Errorf("ssh call %d had the session in its environment: %q", i+1, v)
		}
	}
	for _, c := range calls {
		for _, w := range []string{"VAULT_", "SendEnv", fixAddr, fixTok} {
			if strings.Contains(c.argv, w) || strings.Contains(c.stdin, w) {
				t.Errorf("an ssh call carries %q: %q", w, c.argv)
			}
		}
	}
	noValueAnywhere(t, r, e.stub)
}

// noSession removes the session from the environment the script gets.
func noSession(env []string) []string { return without(without(env, "VAULT_ADDR"), "VAULT_TOKEN") }

// withSession adds a (fake) session to an environment list.
func withSession(env []string) []string { return append(append([]string{}, env...), vault()...) }

// No script puts the session on a remote command line or sends it by ssh.
func TestScriptsNeverSendTheSession(t *testing.T) {
	files, _ := filepath.Glob("*.sh")
	for _, f := range files {
		for i, l := range strings.Split(readText(t, f), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				continue
			}
			if strings.Contains(l, "SendEnv") || regexp.MustCompile(`ssh .*VAULT_|VAULT_[A-Z]*=\S* *(ssh|scp)`).MatchString(l) {
				t.Errorf("%s:%d sends the session over ssh: %s", f, i+1, l)
			}
		}
	}
}

// A bao read that needs the session and has none stops by name, before it
// calls bao, copies, builds or restarts anything.
func TestSessionMissingStopsByName(t *testing.T) {
	want := func(root string, name string) string {
		return name + " is not set (the environment, or .env in " + root + " when run through just)"
	}
	t.Run("cert", func(t *testing.T) {
		e := newCertEnv(t, "role: workstation\nname: jetfire\n")
		e.env = noSession(e.env)
		r := e.run(t, nil, "jetfire")
		if r.code == 0 || !strings.Contains(r.stderr, want(e.wd, "VAULT_ADDR")) {
			t.Fatalf("exit %d: %q", r.code, r.stderr)
		}
		if _, err := os.Stat(filepath.Join(e.stub, "bao.argv")); err == nil {
			t.Error("bao called without a session")
		}
		r = e.run(t, []string{"VAULT_ADDR=" + fixAddr}, "jetfire")
		if r.code == 0 || !strings.Contains(r.stderr, want(e.wd, "VAULT_TOKEN")) {
			t.Fatalf("exit %d: %q", r.code, r.stderr)
		}
	})
	t.Run("install", func(t *testing.T) {
		e, _, env := installEnv(t)
		env = noSession(append(env, "VMTOOL_DOMAIN=example.lab"))
		r := runScript(t, e.stub, e.wd, "install.sh", env)
		if r.code == 0 || !strings.Contains(r.stderr, want(e.wd, "VAULT_ADDR")) {
			t.Fatalf("exit %d: %q", r.code, r.stderr)
		}
		for _, f := range []string{"go.argv", "sudo.argv", "systemctl.argv", "bao.argv"} {
			if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
				t.Errorf("%s exists: install went on", f)
			}
		}
	})
	t.Run("deploy", func(t *testing.T) {
		e, _, _ := newRemoteEnv(t)
		e.env = noSession(append(e.env, "VMTOOL_DOMAIN=example.lab", "BAO_STORE_HOST_TOKENS_PATH=kv/example/hosts"))
		r := e.deploy(t, nil, "hv1")
		if r.code == 0 || !strings.Contains(r.stderr, want(e.wd, "VAULT_ADDR")) {
			t.Fatalf("exit %d: %q", r.code, r.stderr)
		}
		for _, f := range []string{"ssh.argv", "sudo.argv", "bao.argv", "tar.stdin"} {
			if _, err := os.Stat(filepath.Join(e.stub, f)); err == nil {
				t.Errorf("%s exists: deploy went on", f)
			}
		}
	})
	t.Run("token-fetch", func(t *testing.T) {
		stub := stubBin(t, "bao", "exit 0")
		wd := t.TempDir()
		r := runScript(t, stub, wd, "token.sh", nil, "fetch")
		if r.code == 0 || !strings.Contains(r.stderr, want(wd, "VAULT_ADDR")) {
			t.Fatalf("exit %d: %q", r.code, r.stderr)
		}
	})
}

// A failed signing names .env as a place the session comes from.
func TestCertSigningHintNamesDotEnv(t *testing.T) {
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	addStub(t, e.stub, "bao", "exit 1")
	r := e.run(t, vault(), "jetfire")
	if r.code == 0 || !strings.Contains(r.stderr, "signing failed") || !strings.Contains(r.stderr, ".env") {
		t.Errorf("exit %d: %q", r.code, r.stderr)
	}
}

// deploy.env is not a source for the session (it holds no secret).
func TestDeployEnvIsNotASessionSource(t *testing.T) {
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	if err := os.WriteFile(filepath.Join(e.wd, "deploy.env"), []byte("VAULT_ADDR="+fixAddr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.run(t, nil, "jetfire")
	if r.code == 0 || !strings.Contains(r.stderr, "deploy.env line 1") {
		t.Errorf("exit %d: %q", r.code, r.stderr)
	}
}

// just settings lists the session as set or unset and where from, never a
// value; it sources nothing and calls no bao.
func TestSettingsSessionRows(t *testing.T) {
	line := func(out, name string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, name+" ") {
				return l
			}
		}
		t.Fatalf("no row for %s in:\n%s", name, out)
		return ""
	}
	stub := stubBin(t, "bao", "exit 0")
	wd := t.TempDir()
	r := runScript(t, stub, wd, "settings.sh", blankEnv())
	for _, n := range []string{"VAULT_ADDR", "VAULT_TOKEN"} {
		if l := line(r.stdout, n); !strings.Contains(l, "unset") {
			t.Errorf("nothing set: %q", l)
		}
	}
	// ADDR in the environment, TOKEN in .env (export form); .env also holds a
	// command that must not run.
	if err := os.WriteFile(filepath.Join(wd, ".env"), []byte("touch sourced-marker\nexport VAULT_TOKEN="+fixTok+"\nVMTOOL_TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runScript(t, stub, wd, "settings.sh", append(blankEnv(), "VAULT_ADDR="+fixAddr))
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if l := line(r.stdout, "VAULT_ADDR"); !strings.Contains(l, "set (environment)") {
		t.Errorf("VAULT_ADDR: %q", l)
	}
	if l := line(r.stdout, "VAULT_TOKEN"); !strings.Contains(l, "set (.env)") {
		t.Errorf("VAULT_TOKEN: %q", l)
	}
	// Plain form in .env, and an empty one is not set.
	if err := os.WriteFile(filepath.Join(wd, ".env"), []byte("VAULT_ADDR="+fixAddr+"\nVAULT_TOKEN=\"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runScript(t, stub, wd, "settings.sh", blankEnv())
	if l := line(r.stdout, "VAULT_ADDR"); !strings.Contains(l, "set (.env)") {
		t.Errorf("VAULT_ADDR plain: %q", l)
	}
	if l := line(r.stdout, "VAULT_TOKEN"); !strings.Contains(l, "unset") {
		t.Errorf("VAULT_TOKEN empty: %q", l)
	}
	if _, err := os.Stat(filepath.Join(wd, "sourced-marker")); err == nil {
		t.Error("settings sourced .env")
	}
	if _, err := os.Stat(filepath.Join(stub, "bao.argv")); err == nil {
		t.Error("settings called bao")
	}
	for _, v := range []string{fixAddr, fixTok, "FIXADDR"} {
		if strings.Contains(r.stdout+r.stderr, v) {
			t.Errorf("a value leaked: %s%s", r.stdout, r.stderr)
		}
	}
}
