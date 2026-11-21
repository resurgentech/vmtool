package server

import (
	"context"
	"log/slog"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/buildinfo"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/storeclient"
)

// Kicker is what the request log kicks after a VM change: the inventory
// reporter, whose Kick never blocks.
type Kicker interface{ Kick() }

// vmChangeOps are the operations whose success changes what a VM looks like
// in the store's inventory (state, disks, comment, autostart, existence). The
// server kicks the reporter after a 2xx answer of one of them. Every
// operation of vmtool.yaml is here or in noVMChange (inventory_test.go), so
// a new route cannot be forgotten.
var vmChangeOps = map[string]bool{
	"defineVM": true, "deleteVMDefinition": true, "deleteVM": true,
	"startVM": true, "stopVM": true, "resumeVM": true, "poweroffVM": true, "rebootVM": true,
	"setAutostart": true, "setVMComment": true,
	"addDisk": true, "migrateDisk": true, "resizeVMVolume": true, "resizeVolume": true,
}

// startInventory starts the reporter of a hypervisor daemon, or does not: with
// no usable store_url and store_token_file it logs one line naming why (never
// the file's content) and returns nil, and the daemon serves as usual. The
// first send happens in the background. opts configure the store client
// (tests inject their CA). The returned channel closes when the reporter has
// stopped, after ctx ends.
func startInventory(ctx context.Context, cfg *config.Config, r *hosts.Resolver, log *slog.Logger, opts ...storeclient.Option) (*app.InventoryReporter, <-chan struct{}) {
	if cfg == nil || cfg.Role != config.RoleHypervisor {
		return nil, nil
	}
	tok, err := cfg.StoreHostToken()
	if err != nil {
		log.Info("inventory reporting is off: " + err.Error())
		return nil, nil
	}
	client, err := storeclient.New(cfg.StoreURL, tok, opts...)
	if err != nil {
		log.Info("inventory reporting is off: store_url is not usable")
		return nil, nil
	}
	rep := app.NewInventoryReporter(app.InventoryReporterConfig{
		Host: r.Local(), Version: buildinfo.Version(), Store: client, Source: r.InventorySource(), Log: log,
	})
	return rep, rep.Start(ctx)
}
