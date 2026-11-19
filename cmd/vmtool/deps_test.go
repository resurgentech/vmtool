package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The client binary never links the store's server side: no object-storage
// client, no SQLite, none of internal/store. It talks to the store over HTTP
// through internal/storeclient only.
func TestNoStoreServerDeps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	var sawClient bool
	for _, p := range strings.Fields(string(out)) {
		switch {
		case strings.HasPrefix(p, "github.com/minio/minio-go"),
			strings.HasPrefix(p, "modernc.org/sqlite"),
			p == "github.com/jaredeh/vmtool/internal/store",
			strings.HasPrefix(p, "github.com/jaredeh/vmtool/internal/store/"),
			p == "github.com/jaredeh/vmtool/cmd/vmtool-store":
			t.Errorf("vmtool depends on %s", p)
		case p == "github.com/jaredeh/vmtool/internal/storeclient":
			sawClient = true
		}
	}
	if !sawClient {
		t.Error("vmtool does not link internal/storeclient")
	}
}
