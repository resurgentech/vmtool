package scripts_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const fakePath = "kv/infra/labtwo/vmtool/token"

// stubBin writes an executable stub called name into a fresh dir. Each call
// appends its argv (space-joined) to <dir>/<name>.argv; a "kv put" call also
// has its stdin saved to <dir>/<name>.stdin. body is extra shell run after
// recording, to steer behaviour. Returns the dir.
func stubBin(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	addStub(t, dir, name, body)
	return dir
}

// addStub writes one more stub (same recording behaviour) into dir.
func addStub(t *testing.T, dir, name, body string) {
	t.Helper()
	script := "#!/usr/bin/env bash\n" +
		"d=\"$(dirname \"$0\")\"\n" +
		"echo \"$*\" >> \"$d/" + name + ".argv\"\n" +
		"if [[ \"$1 $2\" == \"kv put\" ]]; then cat > \"$d/" + name + ".stdin\"; fi\n" +
		body + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readOr(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

type result struct {
	stdout, stderr string
	code           int
}

// runScript runs scripts/<script> <args> with cwd=workdir, the stub dir first
// on PATH, VMTOOL_ROOT=workdir (so .env lands there) and a scrubbed
// environment plus the given entries (never the caller's VAULT_*).
func runScript(t *testing.T, stub, workdir, script string, env []string, args ...string) result {
	t.Helper()
	abs, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{abs}, args...)...)
	cmd.Dir = workdir
	cmd.Env = append([]string{
		"PATH=" + stub + ":/usr/bin:/bin",
		"HOME=" + workdir,
		"VMTOOL_ROOT=" + workdir,
		"BAO_TOKEN_PATH=" + fakePath,
	}, env...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return result{so.String(), se.String(), code}
}

var envTokenRe = regexp.MustCompile(`(?m)^VMTOOL_TOKEN=([0-9a-f]{64})$`)

func envToken(t *testing.T, dir string) string {
	t.Helper()
	env := readOr(t, filepath.Join(dir, ".env"))
	m := envTokenRe.FindStringSubmatch(env)
	if m == nil {
		t.Fatalf(".env has no 64-hex VMTOOL_TOKEN line:\n%s", env)
	}
	return m[1]
}

func vault() []string {
	return []string{"VAULT_ADDR=http://127.0.0.1:1", "VAULT_TOKEN=fake-vault-token"}
}

func TestTokenStoresInBao(t *testing.T) {
	stub := stubBin(t, "bao", "exit 0")
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, ".env"), []byte("FOO=bar\nVMTOOL_TOKEN=old\nBAZ=qux"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := runScript(t, stub, wd, "token.sh", vault(), "generate")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	tok := envToken(t, wd)
	env := readOr(t, filepath.Join(wd, ".env"))
	for _, keep := range []string{"FOO=bar\n", "BAZ=qux\n"} {
		if !strings.Contains(env, keep) {
			t.Errorf(".env lost %q: %q", keep, env)
		}
	}
	if strings.Contains(env, "old") || strings.Count(env, "VMTOOL_TOKEN=") != 1 {
		t.Errorf("old token not replaced: %q", env)
	}
	if fi, _ := os.Stat(filepath.Join(wd, ".env")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600 (was 0644)", fi.Mode().Perm())
	}
	if strings.Contains(r.stdout+r.stderr, tok) {
		t.Error("token printed")
	}
	if got := strings.TrimSpace(readOr(t, filepath.Join(stub, "bao.stdin"))); got != tok {
		t.Errorf("bao stdin = %q, want token", got)
	}
	argv := readOr(t, filepath.Join(stub, "bao.argv"))
	if strings.Contains(argv, tok) {
		t.Error("token in bao argv")
	}
	if !strings.Contains(argv, "kv put "+fakePath+" string=-") {
		t.Errorf("bao argv lacks kv put: %q", argv)
	}
	if !strings.Contains(r.stdout, "just install") || !strings.Contains(r.stdout, "just deploy") {
		t.Errorf("final message missing re-run advice: %q", r.stdout)
	}
}

func TestTokenCreatesEnv0600(t *testing.T) {
	stub := stubBin(t, "bao", "exit 0")
	wd := t.TempDir()
	r := runScript(t, stub, wd, "token.sh", nil, "generate")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	tok := envToken(t, wd)
	if fi, _ := os.Stat(filepath.Join(wd, ".env")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if strings.Contains(r.stdout+r.stderr, tok) {
		t.Error("token printed")
	}
	if _, err := os.Stat(filepath.Join(stub, "bao.argv")); err == nil {
		t.Error("bao called without VAULT_ADDR/VAULT_TOKEN")
	}
	if !strings.Contains(strings.ToLower(r.stderr), "warn") {
		t.Errorf("no warning on stderr: %q", r.stderr)
	}
}

func TestTokenBaoDownWarns(t *testing.T) {
	stub := stubBin(t, "bao", `if [[ "$1" == status ]]; then exit 2; fi`)
	wd := t.TempDir()
	r := runScript(t, stub, wd, "token.sh", vault(), "generate")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	envToken(t, wd)
	if strings.Contains(readOr(t, filepath.Join(stub, "bao.argv")), "kv put") {
		t.Error("kv put attempted although bao status failed")
	}
	if !strings.Contains(strings.ToLower(r.stderr), "warn") {
		t.Errorf("no warning: %q", r.stderr)
	}
}

func TestTokenFetch(t *testing.T) {
	const known = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	stub := stubBin(t, "bao", `if [[ "$1 $2" == "kv get" ]]; then echo `+known+`; fi`)
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, ".env"), []byte("KEEP=1\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	r := runScript(t, stub, wd, "token.sh", vault(), "fetch")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if got := envToken(t, wd); got != known {
		t.Errorf(".env token %q", got)
	}
	if !strings.Contains(readOr(t, filepath.Join(wd, ".env")), "KEEP=1\n") {
		t.Error("unrelated line lost")
	}
	if fi, _ := os.Stat(filepath.Join(wd, ".env")); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if strings.Contains(r.stdout+r.stderr, known) {
		t.Error("token printed")
	}
	argv := readOr(t, filepath.Join(stub, "bao.argv"))
	if !strings.Contains(argv, "kv get -field=string "+fakePath) {
		t.Errorf("argv: %q", argv)
	}
}

func TestTokenFetchErrors(t *testing.T) {
	good := `if [[ "$1 $2" == "kv get" ]]; then echo abc; fi`
	cases := []struct {
		name string
		body string
		env  []string
		want string
	}{
		{"no addr", good, []string{"VAULT_TOKEN=x"}, "VAULT_ADDR"},
		{"no token", good, []string{"VAULT_ADDR=http://127.0.0.1:1"}, "VAULT_TOKEN"},
		{"get fails", `if [[ "$1" == kv ]]; then echo denied >&2; exit 2; fi`, vault(), "bao"},
		{"empty", `if [[ "$1" == kv ]]; then exit 0; fi`, vault(), "empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stub := stubBin(t, "bao", c.body)
			wd := t.TempDir()
			r := runScript(t, stub, wd, "token.sh", c.env, "fetch")
			if r.code == 0 {
				t.Fatalf("exit 0, want failure: %s%s", r.stdout, r.stderr)
			}
			if !strings.Contains(r.stderr, c.want) {
				t.Errorf("stderr lacks %q: %q", c.want, r.stderr)
			}
			if _, err := os.Stat(filepath.Join(wd, ".env")); err == nil {
				t.Error(".env written on failure")
			}
		})
	}
}

func TestScriptsSyntax(t *testing.T) {
	files, _ := filepath.Glob("*.sh")
	if len(files) == 0 {
		t.Fatal("no scripts")
	}
	for _, f := range files {
		if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", f, err, out)
		}
	}
}

func TestJustSummary(t *testing.T) {
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just not installed")
	}
	out, err := exec.Command("just", "--justfile", "../justfile", "--summary").Output()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, r := range strings.Fields(string(out)) {
		have[r] = true
	}
	for _, want := range []string{"token", "token-fetch", "cert", "install", "deploy", "prereqs"} {
		if !have[want] {
			t.Errorf("just --summary lacks %q: %s", want, out)
		}
	}
}

// A bare `just` must only list recipes; it must never be a recipe with side
// effects such as `token` (the first recipe in the file is the default).
func TestJustDefaultIsList(t *testing.T) {
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just not installed")
	}
	out, err := exec.Command("just", "--justfile", "../justfile", "--dry-run").CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "just --list" {
		t.Errorf("default recipe is not `just --list`: %q", out)
	}
}
