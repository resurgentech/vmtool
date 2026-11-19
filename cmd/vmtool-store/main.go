// Command vmtool-store is the image and credential store. It must stay
// libvirt-free: it imports neither pkg/vmtool nor internal/config, and builds
// with CGO_ENABLED=0.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jaredeh/vmtool/internal/store"
	"github.com/jaredeh/vmtool/internal/store/blob"
	"github.com/jaredeh/vmtool/internal/store/db"
)

func main() {
	fs := flag.NewFlagSet("vmtool-store", flag.ContinueOnError)
	cfgPath := fs.String("config", store.DefaultConfigPath, "path to config.yaml")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			os.Exit(0)
		}
		os.Exit(2)
	}
	c, err := store.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vmtool-store:", err)
		os.Exit(1)
	}
	deps, closeDB, err := openDeps(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vmtool-store:", err)
		os.Exit(1)
	}
	defer closeDB()
	slog.Info("vmtool-store listening", "listen", c.Listen, "tls", c.UsesTLS(), "behind_proxy", c.BehindProxy)
	if err := store.NewServer(c, nil, deps).ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "vmtool-store:", err)
		os.Exit(1)
	}
}

// openDeps opens the encryption key, the database and the object store, and
// makes sure the bucket exists. Errors never include key or credential text.
func openDeps(c *store.Config) (store.Deps, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key, err := db.LoadKey(c.KeyFile)
	if err != nil {
		return store.Deps{}, nil, err
	}
	d, err := db.Open(ctx, filepath.Join(c.DataDir, "store.db"), key)
	if err != nil {
		return store.Deps{}, nil, err
	}
	b, err := blob.NewS3(blob.S3Config{Endpoint: c.S3Endpoint, Bucket: c.Bucket, AccessKey: c.S3AccessKey, SecretKey: c.S3SecretKey})
	if err != nil {
		_ = d.Close()
		return store.Deps{}, nil, err
	}
	if err := b.EnsureBucket(ctx); err != nil {
		_ = d.Close()
		return store.Deps{}, nil, fmt.Errorf("object store: %w", err)
	}
	return store.Deps{Blob: b, DB: d}, func() { _ = d.Close() }, nil
}
