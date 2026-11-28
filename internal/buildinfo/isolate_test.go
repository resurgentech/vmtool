package buildinfo

import (
	"testing"

	"github.com/resurgentech/vmtool/internal/testenv"
)

// TestMain keeps every test off the developer's own key file, token settings
// and secret-store session.
func TestMain(m *testing.M) { testenv.Run(m) }

func TestEnvironmentIsIsolated(t *testing.T) { testenv.Check(t) }
