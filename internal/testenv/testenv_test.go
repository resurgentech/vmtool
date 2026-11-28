package testenv

import (
	"os"
	"testing"
)

func TestIsolateReplacesHomeAndUnsetsTokenVariables(t *testing.T) {
	for _, k := range append([]string{"HOME", "XDG_CONFIG_HOME"}, Unset...) {
		t.Setenv(k, "/poison/"+k) // restored when the test ends
	}
	dir, cleanup := Isolate()
	defer cleanup()
	Check(t)
	if os.Getenv("HOME") != dir {
		t.Errorf("HOME = %q, want %q", os.Getenv("HOME"), dir)
	}
	cleanup()
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("cleanup left %s", dir)
	}
}
