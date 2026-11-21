package server

import (
	"context"
	"log/slog"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// startNetLinks starts the link keeper of a hypervisor daemon, or does not:
// with no usable store_url and store_token_file it logs one line naming why
// (never the file's content) and returns nil, and the daemon serves as usual
// (the links of its networks are then made only by the commands that join and
// leave them). The first pass happens in the background, so serving is never
// delayed; watch registers libvirt's network-started events (nil: none; tests
// inject a fake, the daemon passes vmtool.WatchNetworkStarted). opts configure
// the store client. The returned channel closes when the keeper has stopped,
// after ctx ends.
func startNetLinks(ctx context.Context, cfg *config.Config, r *hosts.Resolver, log *slog.Logger,
	watch func(func(string)) (func(), error), opts ...storeclient.Option) (*app.NetLinkKeeper, <-chan struct{}) {
	if cfg == nil || cfg.Role != config.RoleHypervisor {
		return nil, nil
	}
	tok, err := cfg.StoreHostToken()
	if err != nil {
		log.Info("network links are not kept up from the store: " + err.Error())
		return nil, nil
	}
	client, err := storeclient.New(cfg.StoreURL, tok, opts...)
	if err != nil {
		log.Info("network links are not kept up from the store: store_url is not usable")
		return nil, nil
	}
	k := app.NewNetLinkKeeper(app.NetLinkKeeperConfig{
		Host:  r.Local(),
		Store: client,
		Open:  func() (app.Manager, func(), error) { return r.Resolve(r.Local()) },
		Watch: watch,
		Log:   log,
	})
	return k, k.Start(ctx)
}

// watchNetworkStarted is libvirt's network-started events for the keeper.
var watchNetworkStarted = vmtool.WatchNetworkStarted
