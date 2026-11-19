package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The store never links libvirt: it must build without cgo and have no
// libvirt.org package in its dependency tree.
func TestNoLibvirtDeps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-deps", ".")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, p := range strings.Fields(string(out)) {
		if strings.Contains(p, "libvirt.org") {
			t.Errorf("dependency %s", p)
		}
		if p == "github.com/jaredeh/vmtool/pkg/vmtool" || p == "github.com/jaredeh/vmtool/internal/config" {
			t.Errorf("dependency %s pulls in libvirt", p)
		}
	}
}

func TestBuildsWithoutCgo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "vmtool-store")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	out, err := exec.CommandContext(ctx2, bin, "--help").CombinedOutput()
	if err != nil && !strings.Contains(string(out), "-config") {
		t.Fatalf("--help: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "-config") {
		t.Fatalf("--help does not mention -config:\n%s", out)
	}
}
