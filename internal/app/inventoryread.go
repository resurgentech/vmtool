package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/resurgentech/vmtool/internal/storeclient"
)

// The store's inventory as the read surfaces show it. Types are the store
// client's (the store's own API types).
type (
	Inventory = storeclient.Inventory
)

// errNoStoreConfigured is the message when config.yml has no server: block.
var errNoStoreConfigured = errors.New("no store is configured (config.yml has no server: block)")

// inventoryNeedsStore says why the inventory is not shown; every surface
// prints this text (REST 503 carries it).
func inventoryNeedsStore(cause error) error {
	return unavailable("inventory", fmt.Errorf("%v; the inventory is kept by the store, and there is no local copy", cause))
}

// Inventory reads the store's hosts, VMs and machines. host limits hosts and
// VMs to one host (machines are never filtered); gone adds the VMs their host
// no longer has. It needs the store and has no local mode: with none
// configured, or one that does not answer, it fails with KindUnavailable
// (CLI exit 1, REST 503) and one message, defined here. This is the one call
// the CLI, the REST route and the TUI use.
func (s *Service) Inventory(ctx context.Context, host string, gone bool) (*Inventory, error) {
	c, err := s.Store.requireStore(ctx)
	if err != nil {
		return nil, inventoryNeedsStore(err)
	}
	inv, err := c.Inventory(ctx, host, gone)
	if err != nil {
		return nil, inventoryNeedsStore(fmt.Errorf("the store at %s could not be read: %v", s.Store.Host(), err))
	}
	return inv, nil
}
