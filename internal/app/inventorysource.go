package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// InventoryWriter is the part of the store's inventory a client workflow
// touches: delist a VM its host does not have, and remove a deleted VM's row.
// The store implements it (StoreInventory); there is no local source: in
// local mode nothing here is called.
type InventoryWriter interface {
	// Delist marks the live row of (host, name) gone. A store with no live
	// row answers storeclient.ErrNotFound.
	Delist(ctx context.Context, host, name string) (*storeclient.Delisted, error)
	// DeleteRow removes the row (host, name, uuid) at once. A row that is
	// not there answers storeclient.ErrNotFound.
	DeleteRow(ctx context.Context, host, name, uuid string) error
}

// StoreInventory is InventoryWriter over the store's API.
type StoreInventory struct{ Client *storeclient.Client }

func (s StoreInventory) Delist(ctx context.Context, host, name string) (*storeclient.Delisted, error) {
	return s.Client.Delist(ctx, host, name)
}

func (s StoreInventory) DeleteRow(ctx context.Context, host, name, uuid string) error {
	return s.Client.DeleteInventoryRow(ctx, host, name, uuid)
}

// InventoryFor decides the mode (if not yet decided) and returns the store's
// inventory, or nil in local mode (and for a nil session).
func (s *StoreSession) InventoryFor(ctx context.Context) InventoryWriter {
	if s.Mode(ctx) == ModeStore {
		return StoreInventory{Client: s.client}
	}
	return nil
}

// quietInventory is the store's inventory for delist-on-touch, without
// deciding the mode: nil for a nil session, an unusable token, or a mode
// already decided local; otherwise the store's inventory, also while the mode
// is undecided. A store that is down then fails the call, and the caller
// leaves its error as it was: no fallback line is printed for a down store.
func (s *StoreSession) quietInventory() InventoryWriter {
	if s == nil || s.tokenErr != nil || s.client == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decided && s.mode != ModeStore {
		return nil
	}
	return StoreInventory{Client: s.client}
}

// keptUntilDay is the date a delisted VM's login is kept until.
func keptUntilDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// missingVM is the helper every VM-ref workflow runs its error through. It
// changes nothing unless err is a not-found, a store is configured and the
// command is not in local mode (an undecided mode counts: the delist is tried
// without the whoami probe), the host itself says (Info) it has no such VM,
// and the store had a live row for it: then the row is delisted and the error
// says so (same kind, so REST stays 404). A store that cannot be reached, or
// one that had no row, leaves err as it was, and nothing is printed, not even
// the local-mode fallback line: the VM was missing either way. With no store
// configured, or a mode decided local, there is no store call at all.
func (s *Service) missingVM(ctx context.Context, m Manager, ref VMRef, err error) error {
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindNotFound {
		return err
	}
	// The mode is not decided here: a store that is down at this point must
	// print nothing, not even the local-mode fallback line.
	inv := s.Store.quietInventory()
	if inv == nil {
		return err
	}
	// The failed call may have been about a pool or an image: only the host
	// saying the VM itself is missing counts.
	if _, ierr := m.Info(ref.Name); !vmtool.IsNotFound(ierr) {
		return err
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	d, derr := inv.Delist(cctx, ref.Host, ref.Name)
	if derr != nil || d == nil {
		return err
	}
	msg := fmt.Sprintf("%s: %s has no VM named %s. It was removed from the store's inventory", ref, ref.Host, ref.Name)
	if d.HadCredentials {
		msg += fmt.Sprintf("; its login is kept in the store until %s", keptUntilDay(d.CredentialsKeptUntil))
	}
	return &Error{Kind: KindNotFound, Op: ae.Op, Err: errors.New(msg + "."), Output: ae.Output}
}

// dropInventoryRow removes a deleted VM's row from the store's inventory
// (store mode, uuid known). A row the store does not have is fine; a store
// that cannot be reached gets one line, unless note has already said the store
// is down. note may be nil.
func (s *Service) dropInventoryRow(ctx context.Context, ref VMRef, uuid string, note noteFunc) {
	if uuid == "" || !s.storeMode(ctx) {
		return
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	err := s.Store.InventoryFor(ctx).DeleteRow(cctx, ref.Host, ref.Name, uuid)
	if err == nil || errors.Is(err, storeclient.ErrNotFound) || note == nil {
		return
	}
	note(fmt.Sprintf("%s: the store could not be reached; its inventory row for %s was not removed", s.Store.Host(), ref))
}
