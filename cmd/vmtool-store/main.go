// Command vmtool-store is the image and credential store. It must stay
// libvirt-free: it imports neither pkg/vmtool nor internal/config, and builds
// with CGO_ENABLED=0.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/jaredeh/vmtool/internal/store"
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
	slog.Info("vmtool-store listening", "listen", c.Listen, "tls", c.UsesTLS(), "behind_proxy", c.BehindProxy)
	if err := store.NewServer(c, nil).ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "vmtool-store:", err)
		os.Exit(1)
	}
}
