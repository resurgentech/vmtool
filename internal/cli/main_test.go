package cli

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/resurgentech/vmtool/internal/config"
)

// TestMain keeps every test off the developer's own token file and key
// settings: the default token file path is under a temporary home. It also
// keeps them off a vmtool service running on the developer's machine: the
// gate's check of a key by the local service answers "not answering" unless a
// test sets its own localKeyCheck (or realLocalKeyCheck against a listener it
// started).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vmtool-cli-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", dir)
	os.Setenv("XDG_CONFIG_HOME", dir+"/xdg")
	os.Unsetenv("VMTOOL_TOKEN_FILE")
	os.Unsetenv("VMTOOL_KEY_REQUEST_URL")
	localKeyCheck = func(context.Context, *config.Config, string) (int, error) {
		return 0, errors.New("no local service in tests")
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
