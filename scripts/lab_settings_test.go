package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The lab's values are settings (the environment or deploy.env), never a
// default in the justfile or a script. These tests use neutral names only.

const (
	setDomain   = "VMTOOL_DOMAIN"
	setCA       = "BAO_CA_PATH"
	setTokPath  = "BAO_TOKEN_PATH"
	setRegistry = "VMTOOL_IMAGE_REGISTRY"
	setHostToks = "BAO_STORE_HOST_TOKENS_PATH"

	neutralRegistry = "registry.example.lab"
)

func requiredMsg(name string) string {
	return "set " + name + " in the settings file (deploy.env) or the environment"
}

// without returns env minus every entry for name.
func without(env []string, name string) []string {
	var out []string
	for _, v := range env {
		if !strings.HasPrefix(v, name+"=") {
			out = append(out, v)
		}
	}
	return out
}

// noCallsRecorded fails if any of the named stubs recorded a call.
func noCallsRecorded(t *testing.T, stub string, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(stub, n+".argv")); err == nil {
			t.Errorf("%s was called: %q", n, readOr(t, filepath.Join(stub, n+".argv")))
		}
	}
}

// A required setting that is missing (absent or empty) fails by name before
// any build, copy, docker, push, sudo, ssh or restart.
func TestLabSettingsRequiredFailByName(t *testing.T) {
	type runner func(t *testing.T, name string, empty bool) (stub string, r result)
	certRun := func(t *testing.T, name string, empty bool) (string, result) {
		e := newCertEnv(t, "role: workstation\nname: jetfire\n")
		e.env = append(e.env, "VMTOOL_DOMAIN=example.lab", "BAO_CA_PATH=kv/example/ca")
		if empty {
			return e.stub, e.run(t, []string{name + "="}, "hv1")
		}
		e.env = without(e.env, name)
		return e.stub, e.run(t, nil, "hv1")
	}
	installRun := func(t *testing.T, name string, empty bool) (string, result) {
		e, unit, env := installEnv(t)
		env = append(env, "VMTOOL_DOMAIN=example.lab", "BAO_CA_PATH=kv/example/ca")
		if empty {
			env = append(env, name+"=")
		} else {
			env = without(env, name)
		}
		_ = unit
		return e.stub, runScript(t, e.stub, e.wd, "install.sh", env)
	}
	deployRun := func(t *testing.T, name string, empty bool) (string, result) {
		e, _, _ := newRemoteEnv(t)
		e.env = append(e.env, "VMTOOL_DOMAIN=example.lab", "BAO_CA_PATH=kv/example/ca")
		if empty {
			return e.stub, e.deploy(t, []string{name + "="}, "hv1")
		}
		e.env = without(e.env, name)
		return e.stub, e.deploy(t, nil, "hv1")
	}
	fetchRun := func(t *testing.T, name string, empty bool) (string, result) {
		stub := stubBin(t, "bao", `echo abc`)
		wd := t.TempDir()
		env := vault()
		if empty {
			env = append(env, name+"=")
		}
		return stub, runScript(t, stub, wd, "token.sh", env, "fetch")
	}
	storeRun := func(t *testing.T, name string, empty bool) (string, result) {
		stub := stubBin(t, "docker", "exit 0")
		env := []string{}
		if empty {
			env = append(env, name+"=")
		}
		return stub, runScript(t, stub, t.TempDir(), "store-image.sh", env, "v1")
	}
	cases := []struct {
		recipe  string
		run     runner
		setting string
	}{
		{"cert", certRun, setDomain}, {"cert", certRun, setCA},
		{"install", installRun, setDomain}, {"install", installRun, setCA},
		{"deploy", deployRun, setDomain}, {"deploy", deployRun, setCA},
		{"token-fetch", fetchRun, setTokPath},
		{"store-image", storeRun, setRegistry},
	}
	for _, c := range cases {
		for _, empty := range []bool{false, true} {
			if c.recipe == "token-fetch" && !empty {
				continue // runScript always sets BAO_TOKEN_PATH; only the empty case can be built
			}
			name := c.recipe + "/" + c.setting + "/absent"
			if empty {
				name = c.recipe + "/" + c.setting + "/empty"
			}
			t.Run(name, func(t *testing.T) {
				stub, r := c.run(t, c.setting, empty)
				if r.code == 0 {
					t.Fatalf("exit 0, want failure: %s%s", r.stdout, r.stderr)
				}
				if !strings.Contains(r.stderr, requiredMsg(c.setting)) {
					t.Errorf("stderr lacks %q: %q", requiredMsg(c.setting), r.stderr)
				}
				noCallsRecorded(t, stub, "ssh", "scp", "go", "docker", "sudo", "systemctl", "bao", "tar", "curl")
			})
		}
	}
}

// deploy.env supplies a required setting; an empty one in the environment
// wins over the file and still fails by name.
func TestLabSettingsFromDeployEnv(t *testing.T) {
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	e.env = without(without(e.env, setDomain), setCA)
	if err := os.WriteFile(filepath.Join(e.wd, "deploy.env"), []byte("# lab\nVMTOOL_DOMAIN=example.lab\nBAO_CA_PATH='kv/example/ca'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := e.run(t, nil, "jetfire")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(e.wd, "tls", "jetfire.example.lab.crt")); err != nil {
		t.Error(err)
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "bao.argv")), "kv get -field=string kv/example/ca/crt") {
		t.Errorf("bao argv: %q", readOr(t, filepath.Join(e.stub, "bao.argv")))
	}
	r = e.run(t, []string{"VMTOOL_DOMAIN="}, "jetfire")
	if r.code == 0 || !strings.Contains(r.stderr, requiredMsg(setDomain)) {
		t.Errorf("empty env did not win: exit %d %q", r.code, r.stderr)
	}
}

// cert --local-name needs no setting, and a missing one is not asked for.
func TestCertLocalNameNeedsNoSetting(t *testing.T) {
	e := newCertEnv(t, "role: workstation\nname: jetfire\n")
	e.env = nil
	r := e.run(t, nil, "--local-name")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "jetfire" {
		t.Fatalf("exit %d out %q err %q", r.code, r.stdout, r.stderr)
	}
}

// deploy.env values are validated as plain strings.
func TestLabSettingsUnsafeValuesRefused(t *testing.T) {
	for _, env := range [][]string{
		{"VMTOOL_DOMAIN=a;touch$IFS/tmp/p"},
		{"BAO_CA_PATH=-field=x"},
		{"BAO_STORE_HOST_TOKENS_PATH=kv/x y"},
		{"BAO_TOKEN_PATH=$(id)"},
	} {
		e, _, _ := newRemoteEnv(t)
		e.env = append(e.env, "VMTOOL_DOMAIN=example.lab", "BAO_CA_PATH=kv/example/ca")
		r := e.deploy(t, env, "hv1")
		if r.code == 0 {
			t.Errorf("%v accepted", env)
		}
		noCallsRecorded(t, e.stub, "ssh", "scp", "go", "sudo", "tar")
	}
	stub := stubBin(t, "docker", "exit 0")
	for _, reg := range []string{"reg istry", "registry.example.lab;id", "-x", "reg/$(id)"} {
		r := runScript(t, stub, t.TempDir(), "store-image.sh", []string{"VMTOOL_IMAGE_REGISTRY=" + reg}, "v1")
		if r.code == 0 {
			t.Errorf("registry %q accepted", reg)
		}
	}
	noCallsRecorded(t, stub, "docker")
}

// deploy.env accepts the new keys and still rejects an unknown one, naming the
// line number and never its content.
func TestDeployEnvFileNewKeys(t *testing.T) {
	e, _, _ := newRemoteEnv(t)
	e.env = without(without(e.env, setDomain), setCA)
	writeDeployEnvFile(t, e, "VMTOOL_DOMAIN=example.lab\nBAO_CA_PATH=kv/example/ca\nBAO_STORE_HOST_TOKENS_PATH=kv/example/hosttokens\nBAO_TOKEN_PATH=kv/example/token\nVMTOOL_IMAGE_REGISTRY="+neutralRegistry+"\n")
	r := e.deploy(t, nil, "hv1")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(readOr(t, filepath.Join(e.stub, "remote-vmtool.argv")), "--tls-cert tls/hv1.example.lab.crt") {
		t.Errorf("domain not from the file: %q", readOr(t, filepath.Join(e.stub, "remote-vmtool.argv")))
	}
	writeDeployEnvFile(t, e, "VMTOOL_DOMAIN=example.lab\nVMTOOL_OTHER=SECRETSECRET\n")
	r = e.deploy(t, nil, "hv1")
	if r.code == 0 || !strings.Contains(r.stderr, "deploy.env line 2") || strings.Contains(r.stderr, "SECRETSECRET") {
		t.Errorf("exit %d stderr %q", r.code, r.stderr)
	}
}

// Without BAO_STORE_HOST_TOKENS_PATH deploy reads no host token (no bao call
// for it), warns once, names no other repo, and goes on.
func TestDeployHostTokensPathUnset(t *testing.T) {
	e, _, _ := hostTokenEnv(t, true)
	e.env = without(e.env, setHostToks)
	r := e.deploy(t, nil, "veritech01")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "bao.argv")), "-field=veritech01") {
		t.Errorf("bao asked for a host token: %q", readOr(t, filepath.Join(e.stub, "bao.argv")))
	}
	if n := strings.Count(r.stderr, "veritech01 has no store host token; its daemon will not report VMs"); n != 1 {
		t.Errorf("warning count %d: %s", n, r.stderr)
	}
	if strings.Contains(r.stderr, "make_secrets") {
		t.Errorf("warning names another repo's script: %s", r.stderr)
	}
	if strings.Contains(readOr(t, filepath.Join(e.stub, "remote-vmtool.argv")), "store-token-file") {
		t.Error("config write names a token file")
	}
}

// token without BAO_TOKEN_PATH writes .env and calls no bao; fetch needs it.
func TestTokenWithoutPathWritesEnvOnly(t *testing.T) {
	stub := stubBin(t, "bao", "exit 0")
	wd := t.TempDir()
	r := runScript(t, stub, wd, "token.sh", append(vault(), "BAO_TOKEN_PATH="), "generate")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	tok := envToken(t, wd)
	noCallsRecorded(t, stub, "bao")
	if !strings.Contains(r.stdout+r.stderr, "BAO_TOKEN_PATH") || !strings.Contains(r.stdout, "NOT in OpenBao") {
		t.Errorf("no word on the missing setting: %q %q", r.stdout, r.stderr)
	}
	if strings.Contains(r.stdout+r.stderr, tok) {
		t.Error("token printed")
	}
}

// With neutral values the store-image command lines are today's, with only
// the registry different; tags are checked first.
func TestStoreImageScript(t *testing.T) {
	stub := stubBin(t, "docker", "exit 0")
	wd := t.TempDir()
	r := runScript(t, stub, wd, "store-image.sh", []string{"VMTOOL_IMAGE_REGISTRY=" + neutralRegistry}, "v1.2")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	want := "build -f deploy/vmtool-store.Dockerfile -t registry.example.lab/vmtool-store:v1.2 -t registry.example.lab/vmtool-store:latest .\n" +
		"push registry.example.lab/vmtool-store:v1.2\n" +
		"push registry.example.lab/vmtool-store:latest\n"
	if got := readOr(t, filepath.Join(stub, "docker.argv")); got != want {
		t.Errorf("docker calls:\n%s\nwant:\n%s", got, want)
	}
	for _, tag := range []string{"", "latest", "v1 -x", "-v"} {
		stub := stubBin(t, "docker", "exit 0")
		r := runScript(t, stub, t.TempDir(), "store-image.sh", []string{"VMTOOL_IMAGE_REGISTRY=" + neutralRegistry}, tag)
		if r.code != 2 {
			t.Errorf("tag %q: exit %d, want 2", tag, r.code)
		}
		noCallsRecorded(t, stub, "docker")
	}
	// The registry from deploy.env.
	stub = stubBin(t, "docker", "exit 0")
	wd = t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "deploy.env"), []byte("VMTOOL_IMAGE_REGISTRY=registry.example.lab:5000/team\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := runScript(t, stub, wd, "store-image.sh", nil, "v3"); r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(readOr(t, filepath.Join(stub, "docker.argv")), "push registry.example.lab:5000/team/vmtool-store:v3\n") {
		t.Errorf("docker argv: %q", readOr(t, filepath.Join(stub, "docker.argv")))
	}
}

// settings.sh: names only, set or unset, which recipes an unset one blocks.
var allSettingNames = []string{"VMTOOL_DOMAIN", "BAO_CA_PATH", "BAO_STORE_HOST_TOKENS_PATH", "BAO_TOKEN_PATH",
	"VMTOOL_IMAGE_REGISTRY", "VMTOOL_PERSONAL_KEYS_URL", "VMTOOL_KEY_REQUEST_URL", "BAO_KEYS_PULL_TOKEN_PATH"}

func settingLine(t *testing.T, out, name string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == name {
			return l
		}
	}
	t.Fatalf("no line for %s in:\n%s", name, out)
	return ""
}

func blankEnv() []string {
	var env []string
	for _, n := range allSettingNames {
		env = append(env, n+"=")
	}
	return env
}

func TestSettingsNothingSet(t *testing.T) {
	r := runScript(t, t.TempDir(), t.TempDir(), "settings.sh", blankEnv())
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	for name, blocks := range map[string][]string{
		"VMTOOL_DOMAIN":         {"cert", "install", "deploy"},
		"BAO_CA_PATH":           {"cert", "install", "deploy"},
		"BAO_TOKEN_PATH":        {"token-fetch"},
		"VMTOOL_IMAGE_REGISTRY": {"store-image"},
	} {
		l := settingLine(t, r.stdout, name)
		if !strings.Contains(l, "unset") {
			t.Errorf("%s: %q", name, l)
		}
		for _, b := range blocks {
			if !strings.Contains(l, b) {
				t.Errorf("%s line lacks blocked recipe %s: %q", name, b, l)
			}
		}
	}
	for _, n := range allSettingNames {
		if l := settingLine(t, r.stdout, n); !strings.Contains(l, "unset") || strings.Contains(l, " set ") {
			t.Errorf("%s: %q", n, l)
		}
	}
}

func TestSettingsAllSetNeverPrintsValues(t *testing.T) {
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "deploy.env"), []byte("BAO_CA_PATH=kv/example/FILEVALUEca\nVMTOOL_KEY_REQUEST_URL=https://keys.example.lab/FILEVALUEreq\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"VMTOOL_DOMAIN=ENVVALUE.example.lab", "BAO_STORE_HOST_TOKENS_PATH=kv/example/ENVVALUEhost",
		"BAO_TOKEN_PATH=kv/example/ENVVALUEtoken", "VMTOOL_IMAGE_REGISTRY=ENVVALUE.example.lab",
		"VMTOOL_PERSONAL_KEYS_URL=https://keys.example.lab/ENVVALUEkeys", "BAO_KEYS_PULL_TOKEN_PATH=kv/example/ENVVALUEpull"}
	r := runScript(t, t.TempDir(), wd, "settings.sh", env)
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	for _, n := range allSettingNames {
		if l := settingLine(t, r.stdout, n); !regexp.MustCompile(`\bset\b`).MatchString(l) || strings.Contains(l, "unset") {
			t.Errorf("%s: %q", n, l)
		}
	}
	if !strings.Contains(settingLine(t, r.stdout, "BAO_CA_PATH"), "deploy.env") ||
		!strings.Contains(settingLine(t, r.stdout, "VMTOOL_DOMAIN"), "environment") {
		t.Errorf("sources not shown:\n%s", r.stdout)
	}
	for _, bad := range []string{"VALUE", "example.lab", "kv/example"} {
		if strings.Contains(r.stdout+r.stderr, bad) {
			t.Errorf("a value leaked (%q):\n%s%s", bad, r.stdout, r.stderr)
		}
	}
}

// An empty variable in the environment wins over the file: unset.
func TestSettingsEmptyEnvWinsOverFile(t *testing.T) {
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "deploy.env"), []byte("VMTOOL_DOMAIN=example.lab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runScript(t, t.TempDir(), wd, "settings.sh", blankEnv())
	if l := settingLine(t, r.stdout, "VMTOOL_DOMAIN"); !strings.Contains(l, "unset") {
		t.Errorf("%q", l)
	}
}

func TestSettingsBadDeployEnvExitsZero(t *testing.T) {
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "deploy.env"), []byte("# x\nSECRETSECRET=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runScript(t, t.TempDir(), wd, "settings.sh", blankEnv())
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout+r.stderr, "line 2") || strings.Contains(r.stdout+r.stderr, "SECRETSECRET") {
		t.Errorf("want the line number only: %s%s", r.stdout, r.stderr)
	}
}

// No lab value in the justfile or a script, and the justfile passes no
// setting explicitly (an explicit empty value would beat deploy.env).
func TestNoLabValuesInJustfileOrScripts(t *testing.T) {
	files, _ := filepath.Glob("*.sh")
	files = append(files, "../justfile")
	re := regexp.MustCompile(`labtwo|registry\.|kv/infra`)
	for _, f := range files {
		for i, l := range strings.Split(readText(t, f), "\n") {
			if re.MatchString(l) {
				t.Errorf("%s:%d names a lab value: %s", f, i+1, l)
			}
		}
	}
}

func TestJustRecipesPassNoSettings(t *testing.T) {
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just not installed")
	}
	for _, args := range [][]string{{"deploy", "h1"}, {"cert", "h1"}, {"install"}, {"token"}, {"token-fetch"}, {"store-image", "v1"}, {"settings"}} {
		out, err := exec.Command("just", append([]string{"--justfile", "../justfile", "--dry-run"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		for _, n := range allSettingNames {
			if strings.Contains(string(out), n+"=") {
				t.Errorf("%v passes %s explicitly: %s", args, n, out)
			}
		}
	}
	out, err := exec.Command("just", "--justfile", "../justfile", "--summary").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(" "+string(out)+" ", " settings ") {
		t.Errorf("no settings recipe: %s", out)
	}
}
