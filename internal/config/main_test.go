package config

import (
	"os"
	"testing"
)

// TestMain keeps every test off the developer's own token file.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vmtool-config-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("XDG_CONFIG_HOME", dir+"/xdg")
	os.Unsetenv(TokenFileEnv)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
