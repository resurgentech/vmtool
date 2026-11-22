// Command vmtool-store is the image and credential store. It must stay
// libvirt-free: it imports neither pkg/vmtool nor internal/config, and builds
// with CGO_ENABLED=0.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/resurgentech/vmtool/internal/store"
	"github.com/resurgentech/vmtool/internal/store/blob"
	"github.com/resurgentech/vmtool/internal/store/db"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, newS3))
}

// newS3 builds the real object store from the config. It makes no request.
func newS3(c *store.Config) (blob.BlobStore, error) {
	return blob.NewS3(blob.S3Config{Endpoint: c.S3Endpoint, Bucket: c.Bucket, AccessKey: c.S3AccessKey, SecretKey: c.S3SecretKey})
}

// run is main with its dependencies passed in. Flags come first, then an
// optional subcommand: none serves, "backup" takes one backup and exits.
func run(args []string, stdout, stderr io.Writer, newBlob func(*store.Config) (blob.BlobStore, error)) int {
	fs := flag.NewFlagSet("vmtool-store", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", store.DefaultConfigPath, "path to config.yaml")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	sub := fs.Args()
	if len(sub) > 1 || (len(sub) == 1 && sub[0] != "backup") {
		fmt.Fprintln(stderr, "vmtool-store: unknown command; the only command is: backup")
		return 2
	}
	c, err := store.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "vmtool-store:", err)
		return 1
	}
	if len(sub) == 1 {
		return runBackup(c, stdout, stderr, newBlob)
	}
	return serve(c, stderr, newBlob)
}

func serve(c *store.Config, stderr io.Writer, newBlob func(*store.Config) (blob.BlobStore, error)) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d, b, closeDB, err := openDeps(ctx, c, newBlob)
	if err != nil {
		fmt.Fprintln(stderr, "vmtool-store:", err)
		return 1
	}
	defer closeDB()
	log := slog.Default()
	slog.Info("vmtool-store listening", "listen", c.Listen, "tls", c.UsesTLS(), "behind_proxy", c.BehindProxy)
	bctx, cancel := context.WithCancel(ctx)
	stopped := store.StartBackups(bctx, store.BackupDeps{DB: d, Blob: b, DataDir: c.DataDir, Log: log})
	swept := store.StartSweeps(bctx, store.SweepDeps{DB: d, Log: log})
	errc := make(chan error, 1)
	go func() { errc <- store.NewServer(c, nil, store.Deps{Blob: b, DB: d}).ListenAndServe() }()
	code := 0
	select {
	case err := <-errc:
		fmt.Fprintln(stderr, "vmtool-store:", err)
		code = 1
	case <-ctx.Done():
	}
	cancel()
	<-stopped
	<-swept
	return code
}

// runBackup takes one backup through its own SQLite connection to the same
// database file. VACUUM INTO is a read transaction, so it runs beside the
// store. It prints the object name and nothing else on success.
func runBackup(c *store.Config, stdout, stderr io.Writer, newBlob func(*store.Config) (blob.BlobStore, error)) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	key, err := db.LoadKey(c.KeyFile)
	if err != nil {
		fmt.Fprintln(stderr, "vmtool-store:", err)
		return 1
	}
	d, err := db.Open(ctx, filepath.Join(c.DataDir, "store.db"), key)
	if err != nil {
		fmt.Fprintln(stderr, "vmtool-store:", err)
		return 1
	}
	defer d.Close()
	b, err := newBlob(c)
	if err != nil {
		// Never print err: a client error can quote the endpoint or a key.
		fmt.Fprintln(stderr, "vmtool-store: cannot set up the object store; check s3_endpoint, bucket and the S3 environment")
		return 1
	}
	name, err := store.RunBackup(ctx, store.BackupDeps{DB: d, Blob: b, DataDir: c.DataDir, Log: slog.New(slog.NewTextHandler(stderr, nil))})
	if err != nil {
		var be *store.BackupError
		if errors.As(err, &be) {
			fmt.Fprintf(stderr, "vmtool-store: %v\n", be)
		} else {
			fmt.Fprintln(stderr, "vmtool-store: backup failed")
		}
		return 1
	}
	fmt.Fprintln(stdout, name)
	return 0
}

// openDeps opens the encryption key, the database and the object store, and
// makes sure the bucket exists. Errors never include key or credential text.
func openDeps(ctx context.Context, c *store.Config, newBlob func(*store.Config) (blob.BlobStore, error)) (*db.DB, blob.BlobStore, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key, err := db.LoadKey(c.KeyFile)
	if err != nil {
		return nil, nil, nil, err
	}
	d, err := db.Open(ctx, filepath.Join(c.DataDir, "store.db"), key)
	if err != nil {
		return nil, nil, nil, err
	}
	b, err := newBlob(c)
	if err != nil {
		_ = d.Close()
		return nil, nil, nil, err
	}
	if err := b.EnsureBucket(ctx); err != nil {
		_ = d.Close()
		return nil, nil, nil, fmt.Errorf("object store: %w", err)
	}
	return d, b, func() { _ = d.Close() }, nil
}
