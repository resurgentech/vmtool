// Package testenv keeps a package's tests off the developer's own key file,
// token settings and secret-store session. It is imported only from tests.
//
// Every test package that can reach the token file, a key, or a built vmtool
// calls Run from its TestMain. Run points HOME and XDG_CONFIG_HOME at a
// temporary directory and unsets the variables that carry a token, a token
// file path or a secret-store address, so a test sees only what it sets
// itself.
package testenv

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Unset lists the variables Isolate removes from the environment.
var Unset = []string{
	"VMTOOL_TOKEN_FILE",
	"VMTOOL_TOKEN",
	"VMTOOL_SERVER_TOKEN",
	"VMTOOL_KEY_REQUEST_URL",
	"VAULT_ADDR",
	"VAULT_TOKEN",
	"BAO_ADDR",
	"BAO_TOKEN",
}

// goVars are the Go settings that default to a place under HOME. They are
// pinned to their current values first, so a go build or go list started by a
// test still finds the developer's caches.
var goVars = []string{"GOCACHE", "GOMODCACHE", "GOPATH"}

// Isolate points HOME at a new temporary directory (and XDG_CONFIG_HOME at
// "xdg" inside it), unsets the variables in Unset and returns the directory
// and a function that removes it.
func Isolate() (dir string, cleanup func()) {
	var missing bool
	for _, k := range goVars {
		if os.Getenv(k) == "" {
			missing = true
		}
	}
	if missing {
		if out, err := exec.Command("go", "env", "GOCACHE", "GOMODCACHE", "GOPATH").Output(); err == nil {
			vals := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
			if len(vals) == len(goVars) {
				for i, k := range goVars {
					if os.Getenv(k) == "" && vals[i] != "" {
						os.Setenv(k, vals[i])
					}
				}
			}
		}
	}
	dir, err := os.MkdirTemp("", "vmtool-test-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("XDG_CONFIG_HOME", dir+"/xdg")
	for _, k := range Unset {
		os.Unsetenv(k)
	}
	return dir, func() { os.RemoveAll(dir) }
}

// Run is the whole TestMain of a package that needs no setup of its own.
func Run(m *testing.M) {
	_, cleanup := Isolate()
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// Check fails t if the process environment is not isolated: HOME is the
// developer's, or one of the Unset variables is set.
func Check(t *testing.T) {
	t.Helper()
	if h := os.Getenv("HOME"); !strings.Contains(h, "vmtool-test-home") {
		t.Errorf("HOME = %q, want a vmtool-test-home temporary directory", h)
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); !strings.Contains(x, "vmtool-test-home") {
		t.Errorf("XDG_CONFIG_HOME = %q, want it under the temporary home", x)
	}
	for _, k := range Unset {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s is set in the test environment", k)
		}
	}
}
