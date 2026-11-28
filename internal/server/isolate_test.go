package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/resurgentech/vmtool/internal/testenv"
)

// TestMain keeps every test off the developer's own key file, token settings
// and secret-store session, and removes the built child binary's directory
// after the run.
func TestMain(m *testing.M) {
	_, cleanup := testenv.Isolate()
	code := m.Run()
	if childBin != "" {
		os.RemoveAll(filepath.Dir(childBin))
	}
	cleanup()
	os.Exit(code)
}

func TestEnvironmentIsIsolated(t *testing.T) { testenv.Check(t) }
