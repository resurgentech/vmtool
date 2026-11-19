package scripts_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Shape checks on the store's container image: the Dockerfile, .dockerignore
// and the `store-image` recipe, all read as text. No docker runs here.

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// dockerLines returns the instruction lines (comments and blanks dropped,
// continuations joined).
func dockerLines(s string) []string {
	s = strings.ReplaceAll(s, "\\\n", " ")
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

func TestStoreImageDockerfileShape(t *testing.T) {
	lines := dockerLines(readText(t, "../deploy/vmtool-store.Dockerfile"))
	var froms, volumes, expose, users []string
	for _, l := range lines {
		f := strings.Fields(l)
		switch strings.ToUpper(f[0]) {
		case "FROM":
			froms = append(froms, l)
		case "VOLUME":
			volumes = append(volumes, l)
		case "EXPOSE":
			expose = append(expose, l)
		case "USER":
			users = append(users, l)
		case "ARG", "ENV":
			if m := regexp.MustCompile(`(?i)token|key|secret|password|pass\b`).FindString(l); m != "" {
				t.Errorf("%q must not carry %q", l, m)
			}
		}
	}
	if len(froms) < 2 {
		t.Fatalf("want a multi-stage build, got FROMs %v", froms)
	}
	if !regexp.MustCompile(`^FROM golang:1\.\d+\.\d+(-\S+)? AS `).MatchString(froms[0]) {
		t.Errorf("builder must be a pinned golang:1.x.y image: %q", froms[0])
	}
	last := froms[len(froms)-1]
	if !strings.Contains(last, "gcr.io/distroless/static-debian12:nonroot") && !strings.HasPrefix(last, "FROM scratch") {
		t.Errorf("runtime must be distroless static (nonroot) or scratch: %q", last)
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{"CGO_ENABLED=0", "./cmd/vmtool-store"} {
		if !strings.Contains(text, want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	if len(users) == 0 {
		t.Fatal("no USER line")
	}
	u := strings.Fields(users[len(users)-1])[1]
	if u == "0" || u == "root" || strings.HasPrefix(u, "0:") || strings.HasPrefix(u, "root:") {
		t.Errorf("runtime user is root: %q", users[len(users)-1])
	}
	if len(expose) != 1 || strings.Fields(expose[0])[1] != "8095" {
		t.Errorf("want exactly EXPOSE 8095, got %v", expose)
	}
	vol := strings.Join(volumes, " ")
	for _, v := range []string{"/var/lib/vmtool-store", "/etc/vmtool-store"} {
		if !strings.Contains(vol, v) {
			t.Errorf("no VOLUME %s (got %q)", v, vol)
		}
	}
	if !strings.Contains(text, "--chown=65532") {
		t.Error("data dir must be copied with --chown to the nonroot uid")
	}
	if !strings.Contains(text, "ENTRYPOINT") || !strings.Contains(text, "/etc/vmtool-store/config.yaml") {
		t.Error("entrypoint must run vmtool-store with the default -config path")
	}
	if strings.Contains(text, "--build-arg") {
		t.Error("no build args")
	}
}

func TestStoreImageDockerignore(t *testing.T) {
	have := map[string]bool{}
	for _, l := range strings.Split(readText(t, "../.dockerignore"), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	for _, want := range []string{".git", ".env", "config.yml", ".machines", "tls", "images", "tmp",
		"packer_images/output", "ROADMAP.md", "docs/reports", ".claude", "vmtool"} {
		if !have[want] && !have[want+"/"] && !have["/"+want] {
			t.Errorf(".dockerignore does not exclude %s", want)
		}
	}
	for _, bad := range []string{"cmd", "internal", "go.mod", "go.sum", "spec"} {
		if have[bad] || have[bad+"/"] {
			t.Errorf(".dockerignore excludes %s, which the build needs", bad)
		}
	}
}

func TestStoreImageRecipe(t *testing.T) {
	jf := readText(t, "../justfile")
	i := strings.Index(jf, "\nstore-image tag:")
	if i < 0 {
		t.Fatal("no `store-image tag` recipe in the justfile")
	}
	body := jf[i+1:]
	if j := strings.Index(body, "\n\n"); j >= 0 {
		body = body[:j]
	}
	for _, want := range []string{
		"docker build",
		"-f deploy/vmtool-store.Dockerfile",
		"-t registry.labtwo/vmtool-store:{{ tag }}",
		"-t registry.labtwo/vmtool-store:latest",
		"docker push registry.labtwo/vmtool-store:{{ tag }}",
		"docker push registry.labtwo/vmtool-store:latest",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("recipe lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "--build-arg") || regexp.MustCompile(`(?i)token|secret|password|login`).MatchString(body) {
		t.Errorf("recipe must carry no build arg or credential:\n%s", body)
	}
	if !strings.Contains(body, "latest") || !regexp.MustCompile(`"\{\{ tag \}\}" *(==|=) *"?(latest|)`).MatchString(body) {
		t.Errorf("recipe must refuse an empty tag and `latest`:\n%s", body)
	}
}
