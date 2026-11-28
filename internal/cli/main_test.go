package cli

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/testenv"
)

// TestMain keeps every test off the developer's own token file and key
// settings: the default token file path is under a temporary home. It also
// keeps them off a vmtool service running on the developer's machine: the
// gate's check of a key by the local service answers "not answering" unless a
// test sets its own localKeyCheck (or realLocalKeyCheck against a listener it
// started).
func TestMain(m *testing.M) {
	_, cleanup := testenv.Isolate()
	localKeyCheck = func(context.Context, *config.Config, string) (int, error) {
		return 0, errors.New("no local service in tests")
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func TestEnvironmentIsIsolated(t *testing.T) { testenv.Check(t) }
