package cli

import (
	"os"
	"testing"
)

// TestMain keeps every test off the developer's own token file and key
// settings: the default token file path is under a temporary home.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vmtool-cli-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("XDG_CONFIG_HOME", dir+"/xdg")
	os.Unsetenv("VMTOOL_TOKEN_FILE")
	os.Unsetenv("VMTOOL_KEY_REQUEST_URL")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
