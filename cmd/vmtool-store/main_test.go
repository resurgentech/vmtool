package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/store"
	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/store/db"
	"github.com/resurgentech/vmtool/internal/tokenhash"
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
		if p == "github.com/resurgentech/vmtool/pkg/vmtool" || p == "github.com/resurgentech/vmtool/internal/config" {
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

// backupFixture writes a config, tokens and key under a temp dir and opens
// the database the way the running store holds it (kept open for the test).
func backupFixture(t *testing.T) (cfgPath, dataDir string, d *db.DB) {
	t.Helper()
	t.Setenv(store.S3AccessKeyEnv, "PLACEHOLDER-KEY-not-a-key")
	t.Setenv(store.S3SecretKeyEnv, "PLACEHOLDER-KEY-not-a-key")
	dir := t.TempDir()
	dataDir = filepath.Join(dir, "data")
	tp, kp := filepath.Join(dir, "tokens.yaml"), filepath.Join(dir, "enc.key")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(tp, []byte("alice: "+tokenhash.Hash("PLACEHOLDER-not-a-secret-1")+"\n"), 0o600))
	must(os.WriteFile(kp, bytes.Repeat([]byte{1}, 32), 0o600))
	cfgPath = filepath.Join(dir, "config.yaml")
	must(os.WriteFile(cfgPath, []byte("tokens_file: "+tp+"\nkey_file: "+kp+"\ndata_dir: "+dataDir+
		"\nbucket: placeholder-bucket\ns3_endpoint: PLACEHOLDER-HOST:9\n"), 0o600))
	d, err := db.Open(context.Background(), filepath.Join(dataDir, "store.db"), bytes.Repeat([]byte{1}, 32))
	must(err)
	t.Cleanup(func() { d.Close() })
	must(d.PutVMCredential(context.Background(), db.VMCredential{Host: "hv1", Name: "vm1",
		UUID: "00000000-0000-4000-8000-000000000001", Username: "root", Password: "PLACEHOLDER-not-a-secret-2"}))
	return cfgPath, dataDir, d
}

func TestBackupSubcommandWritesOneObjectBesideTheRunningStore(t *testing.T) {
	cfg, dataDir, _ := backupFixture(t) // the "running store" still holds its connection
	mem := blob.NewMemory()
	var out, errb bytes.Buffer
	code := run([]string{"-config", cfg, "backup"}, &out, &errb,
		func(*store.Config) (blob.BlobStore, error) { return mem, nil })
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	line := strings.TrimSuffix(out.String(), "\n")
	if strings.Contains(line, "\n") || !strings.HasPrefix(line, "backups/store-") || !strings.HasSuffix(line, ".db") {
		t.Fatalf("stdout = %q, want exactly the object name", out.String())
	}
	if _, err := mem.Stat(context.Background(), line); err != nil {
		t.Fatalf("object %s: %v", line, err)
	}
	ents, _ := os.ReadDir(dataDir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".backup-") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
}

func TestBackupSubcommandFailuresExitNonZeroWithoutSecrets(t *testing.T) {
	cfg, _, _ := backupFixture(t)
	var out, errb bytes.Buffer
	code := run([]string{"-config", cfg, "backup"}, &out, &errb,
		func(*store.Config) (blob.BlobStore, error) {
			return nil, errors.New("dial PLACEHOLDER-KEY-not-a-key refused")
		})
	if code == 0 || out.Len() != 0 {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
	if strings.Contains(errb.String(), "PLACEHOLDER") || errb.Len() == 0 {
		t.Fatalf("stderr = %q", errb.String())
	}
	if code := run([]string{"-config", cfg, "frobnicate"}, &out, &errb, nil); code != 2 {
		t.Fatalf("unknown subcommand exit %d, want 2", code)
	}
}
