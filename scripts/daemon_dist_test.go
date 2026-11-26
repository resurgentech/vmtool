package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The daemon build for any recent glibc: the Dockerfile and the two recipes
// (`daemon-dist`, `daemon-image`), read as text, and the scripts run with a
// stub docker and a stub git. No docker or git runs for real here.

const (
	fakeRev     = "0123456789abcdef0123456789abcdef01234567"
	defaultBase = "ubuntu:22.04"
	distSetting = "VMTOOL_DAEMON_BUILD_BASE"
)

// distFiles is what the build puts in dist/ (SHA256SUMS aside).
var distFiles = []string{"vmtool", "vmtool-net", "vmtool-net.sudoers", "vmtool-thin", "vmtool-thin.sudoers", "vmtool.service.in"}

// daemonStubs makes a stub dir with a docker that, when it is given
// `--output type=local,dest=<dir>`, writes the dist files and a SHA256SUMS
// into <dir> (sums: "all" lists every file, "short" lists only vmtool), and a
// git whose HEAD is fakeRev and whose status output is dirty.
func daemonStubs(t *testing.T, sums, dirty string) string {
	t.Helper()
	stub := t.TempDir()
	docker := `d=""
for a in "$@"; do case "$a" in type=local,dest=*) d="${a#type=local,dest=}";; esac; done
if [[ -n $d ]]; then
  mkdir -p "$d"
  for f in ` + strings.Join(distFiles, " ") + `; do echo "content of $f" > "$d/$f"; done
  if [[ ` + sums + ` == all ]]; then (cd "$d" && sha256sum ` + strings.Join(distFiles, " ") + ` > SHA256SUMS); else (cd "$d" && sha256sum vmtool > SHA256SUMS); fi
fi
exit 0`
	addStub(t, stub, "docker", docker)
	git := `case "$*" in
  *"rev-parse HEAD"*) echo ` + fakeRev + `;;
  *"status --porcelain"*) printf '%s' '` + dirty + `';;
esac
exit 0`
	addStub(t, stub, "git", git)
	return stub
}

func distRun(t *testing.T, stub, script string, env []string, args ...string) (string, result) {
	t.Helper()
	wd := t.TempDir()
	return wd, runScript(t, stub, wd, script, env, args...)
}

func TestDaemonDockerfileShape(t *testing.T) {
	lines := dockerLines(readText(t, "../deploy/vmtool-daemon.Dockerfile"))
	text := strings.Join(lines, "\n")
	var froms []string
	argBeforeFrom := false
	for _, l := range lines {
		f := strings.Fields(l)
		switch strings.ToUpper(f[0]) {
		case "FROM":
			froms = append(froms, l)
		case "ARG":
			if len(froms) == 0 && strings.HasPrefix(f[1], distSetting+"=") {
				argBeforeFrom = true
			}
			if m := regexp.MustCompile(`(?i)token|secret|password|pass\b`).FindString(l); m != "" {
				t.Errorf("%q must not carry %q", l, m)
			}
		}
	}
	if !argBeforeFrom {
		t.Errorf("no `ARG %s=<generic default>` before the first FROM", distSetting)
	}
	want := []string{
		`FROM golang:1.\d+.\d+(-\S+)? AS go`,
		`FROM \$\{` + distSetting + `\} AS build`,
		`FROM scratch AS dist`,
		`FROM scratch AS image`,
	}
	if len(froms) != len(want) {
		t.Fatalf("want %d stages, got %v", len(want), froms)
	}
	for i, w := range want {
		if !regexp.MustCompile("^" + w + "$").MatchString(froms[i]) {
			t.Errorf("stage %d: %q does not match %s", i, froms[i], w)
		}
	}
	for _, w := range []string{"CGO_ENABLED=1", "-tags libvirt_dlopen", "-trimpath", "./cmd/vmtool",
		"internal/buildinfo.stamped=", "sha256sum", "deploy/vmtool-thin", "deploy/vmtool-net", "deploy/vmtool.service.in",
		"deploy/vmtool-thin.sudoers", "deploy/vmtool-net.sudoers"} {
		if !strings.Contains(text, w) {
			t.Errorf("Dockerfile lacks %q", w)
		}
	}
	if !regexp.MustCompile(`COPY --from=build /out/ /dist/`).MatchString(text) {
		t.Error("the image stage does not put the files at /dist")
	}
	// The Go image is pinned to the go line of go.mod or newer within it.
	gomod := readText(t, "../go.mod")
	m := regexp.MustCompile(`(?m)^go (1\.\d+)\.\d+$`).FindStringSubmatch(gomod)
	if m == nil || !strings.Contains(froms[0], "golang:"+m[1]+".") {
		t.Errorf("builder %q is not a golang:%s.x image", froms[0], m)
	}
}

func TestDaemonRecipes(t *testing.T) {
	jf := readText(t, "../justfile")
	for _, c := range []struct{ head, script string }{
		{"\ndaemon-dist:", "scripts/daemon-dist.sh"},
		{"\ndaemon-image tag:", "scripts/daemon-image.sh"},
	} {
		i := strings.Index(jf, c.head)
		if i < 0 {
			t.Fatalf("no %q recipe in the justfile", strings.TrimSpace(c.head))
		}
		body := jf[i+1:]
		if j := strings.Index(body, "\n\n"); j >= 0 {
			body = body[:j]
		}
		if !strings.Contains(body, c.script) {
			t.Errorf("recipe does not run %s:\n%s", c.script, body)
		}
	}
	// The gate also vets with the libvirt_dlopen tag.
	if !regexp.MustCompile(`(?m)^check:.*\bvet-dlopen\b`).MatchString(jf) || !strings.Contains(jf, "go vet -tags libvirt_dlopen ./...") {
		t.Error("`just check` does not run go vet -tags libvirt_dlopen ./...")
	}
	gi := readText(t, "../.gitignore")
	if !regexp.MustCompile(`(?m)^/dist/$`).MatchString(gi) {
		t.Error(".gitignore lacks /dist/")
	}
}

func TestDaemonDistScript(t *testing.T) {
	stub := daemonStubs(t, "all", "")
	wd, r := distRun(t, stub, "daemon-dist.sh", nil)
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv := readOr(t, filepath.Join(stub, "docker.argv"))
	if strings.Count(argv, "\n") != 1 {
		t.Fatalf("want one docker call: %q", argv)
	}
	for _, w := range []string{"build ", "-f deploy/vmtool-daemon.Dockerfile", "--target dist",
		"--build-arg " + distSetting + "=" + defaultBase, "--build-arg VMTOOL_COMMIT=" + fakeRev, "--output type=local,dest="} {
		if !strings.Contains(argv, w) {
			t.Errorf("docker argv lacks %q: %q", w, argv)
		}
	}
	// Every file is in dist/, and SHA256SUMS lists every one of them.
	ents, err := os.ReadDir(filepath.Join(wd, "dist"))
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, e := range ents {
		have = append(have, e.Name())
	}
	sort.Strings(have)
	wantFiles := append([]string{"SHA256SUMS"}, distFiles...)
	sort.Strings(wantFiles)
	if strings.Join(have, " ") != strings.Join(wantFiles, " ") {
		t.Errorf("dist/ holds %v, want %v", have, wantFiles)
	}
	sums := readOr(t, filepath.Join(wd, "dist", "SHA256SUMS"))
	for _, f := range distFiles {
		if !regexp.MustCompile(`(?m)^[0-9a-f]{64}  ` + regexp.QuoteMeta(f) + `$`).MatchString(sums) {
			t.Errorf("SHA256SUMS does not list %s:\n%s", f, sums)
		}
	}
}

func TestDaemonDistBaseSetting(t *testing.T) {
	// From the environment, and from deploy.env; the environment wins.
	stub := daemonStubs(t, "all", "")
	_, r := distRun(t, stub, "daemon-dist.sh", []string{distSetting + "=debian:12"})
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(readOr(t, filepath.Join(stub, "docker.argv")), "--build-arg "+distSetting+"=debian:12 ") {
		t.Errorf("docker argv: %q", readOr(t, filepath.Join(stub, "docker.argv")))
	}
	stub = daemonStubs(t, "all", "")
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "deploy.env"), []byte(distSetting+"=debian:11\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := runScript(t, stub, wd, "daemon-dist.sh", nil); r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(readOr(t, filepath.Join(stub, "docker.argv")), "--build-arg "+distSetting+"=debian:11 ") {
		t.Errorf("docker argv: %q", readOr(t, filepath.Join(stub, "docker.argv")))
	}
	// Unsafe values never reach docker.
	for _, bad := range []string{"deb ian:12", "debian:12;id", "-x", "$(id)"} {
		stub := daemonStubs(t, "all", "")
		_, r := distRun(t, stub, "daemon-dist.sh", []string{distSetting + "=" + bad})
		if r.code == 0 {
			t.Errorf("base %q accepted", bad)
		}
		noCallsRecorded(t, stub, "docker")
	}
}

func TestDaemonDistDirtyTree(t *testing.T) {
	stub := daemonStubs(t, "all", " M internal/cli/cli.go\n")
	wd, r := distRun(t, stub, "daemon-dist.sh", nil)
	if r.code == 0 || !strings.Contains(r.stderr, "VMTOOL_ALLOW_DIRTY") {
		t.Fatalf("dirty tree: exit %d %q", r.code, r.stderr)
	}
	noCallsRecorded(t, stub, "docker")
	if _, err := os.Stat(filepath.Join(wd, "dist")); err == nil {
		t.Error("dist/ was made for a refused build")
	}
	// Told to, it builds and stamps +dirty.
	stub = daemonStubs(t, "all", " M internal/cli/cli.go\n")
	_, r = distRun(t, stub, "daemon-dist.sh", []string{"VMTOOL_ALLOW_DIRTY=1"})
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(readOr(t, filepath.Join(stub, "docker.argv")), "--build-arg VMTOOL_COMMIT="+fakeRev+"+dirty") {
		t.Errorf("docker argv: %q", readOr(t, filepath.Join(stub, "docker.argv")))
	}
}

// A SHA256SUMS that leaves a file out fails the build and leaves no dist/.
func TestDaemonDistChecksIncompleteSums(t *testing.T) {
	stub := daemonStubs(t, "short", "")
	wd, r := distRun(t, stub, "daemon-dist.sh", nil)
	if r.code == 0 || !strings.Contains(r.stderr, "SHA256SUMS") {
		t.Fatalf("exit %d %q", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(wd, "dist")); err == nil {
		t.Error("dist/ was left behind")
	}
}

func TestDaemonImageScript(t *testing.T) {
	reg := []string{"VMTOOL_IMAGE_REGISTRY=" + neutralRegistry}
	stub := daemonStubs(t, "all", "")
	_, r := distRun(t, stub, "daemon-image.sh", reg, "v1.2")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	want := "build -f deploy/vmtool-daemon.Dockerfile --target image --build-arg " + distSetting + "=" + defaultBase +
		" --build-arg VMTOOL_COMMIT=" + fakeRev + " -t registry.example.lab/vmtool-daemon:v1.2 .\n" +
		"push registry.example.lab/vmtool-daemon:v1.2\n"
	if got := readOr(t, filepath.Join(stub, "docker.argv")); got != want {
		t.Errorf("docker calls:\n%s\nwant:\n%s", got, want)
	}
	// The registry from deploy.env; the base from the environment.
	stub = daemonStubs(t, "all", "")
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "deploy.env"), []byte("VMTOOL_IMAGE_REGISTRY=registry.example.lab:5000/team\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := runScript(t, stub, wd, "daemon-image.sh", []string{distSetting + "=debian:12"}, "v3"); r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.stdout, r.stderr)
	}
	argv := readOr(t, filepath.Join(stub, "docker.argv"))
	if !strings.Contains(argv, "push registry.example.lab:5000/team/vmtool-daemon:v3\n") || !strings.Contains(argv, distSetting+"=debian:12 ") {
		t.Errorf("docker argv: %q", argv)
	}
}

func TestDaemonImageRefusals(t *testing.T) {
	reg := []string{"VMTOOL_IMAGE_REGISTRY=" + neutralRegistry}
	for _, tag := range []string{"", "latest", "v1 -x", "-v"} {
		stub := daemonStubs(t, "all", "")
		_, r := distRun(t, stub, "daemon-image.sh", reg, tag)
		if r.code != 2 {
			t.Errorf("tag %q: exit %d, want 2", tag, r.code)
		}
		noCallsRecorded(t, stub, "docker")
	}
	// No registry, absent or empty: by name, before docker.
	for _, env := range [][]string{nil, {"VMTOOL_IMAGE_REGISTRY="}} {
		stub := daemonStubs(t, "all", "")
		_, r := distRun(t, stub, "daemon-image.sh", env, "v1")
		if r.code == 0 || !strings.Contains(r.stderr, requiredMsg(setRegistry)) {
			t.Errorf("env %v: exit %d %q", env, r.code, r.stderr)
		}
		noCallsRecorded(t, stub, "docker")
	}
	// A dirty tree, before docker.
	stub := daemonStubs(t, "all", " M go.mod\n")
	_, r := distRun(t, stub, "daemon-image.sh", reg, "v1")
	if r.code == 0 || !strings.Contains(r.stderr, "VMTOOL_ALLOW_DIRTY") {
		t.Errorf("dirty: exit %d %q", r.code, r.stderr)
	}
	noCallsRecorded(t, stub, "docker")
	// An unsafe registry.
	stub = daemonStubs(t, "all", "")
	_, r = distRun(t, stub, "daemon-image.sh", []string{"VMTOOL_IMAGE_REGISTRY=reg/$(id)"}, "v1")
	if r.code == 0 {
		t.Error("unsafe registry accepted")
	}
	noCallsRecorded(t, stub, "docker")
}

func TestDaemonBuildSettingListed(t *testing.T) {
	r := runScript(t, t.TempDir(), t.TempDir(), "settings.sh", blankEnv())
	l := settingLine(t, r.stdout, distSetting)
	if !strings.Contains(l, "unset") || !strings.Contains(l, "daemon-dist") || !strings.Contains(l, defaultBase) {
		t.Errorf("%q", l)
	}
	// Set in the file: shown as set, no value.
	wd := t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "deploy.env"), []byte(distSetting+"=debian:12\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runScript(t, t.TempDir(), wd, "settings.sh", blankEnv())
	l = settingLine(t, r.stdout, distSetting)
	if !strings.Contains(l, "deploy.env") || strings.Contains(r.stdout, "debian:12") {
		t.Errorf("%q", l)
	}
}

func TestDaemonRecipesPassNoSettings(t *testing.T) {
	if _, err := exec.LookPath("just"); err != nil {
		t.Skip("just not installed")
	}
	for _, args := range [][]string{{"daemon-dist"}, {"daemon-image", "v1"}} {
		out, err := exec.Command("just", append([]string{"--justfile", "../justfile", "--dry-run"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		for _, n := range append([]string{distSetting}, allSettingNames...) {
			if strings.Contains(string(out), n+"=") {
				t.Errorf("%v passes %s explicitly: %s", args, n, out)
			}
		}
	}
}
