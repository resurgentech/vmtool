package app

import (
	"context"
	"fmt"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Metal and alien machines in store mode: the store holds the record (manifest
// and credentials) and .machines/<name>/ is a cache of it, as for VMs, with the
// same .vmtool-source marker (the line is the bare state: machines have no
// UUID). Every command that uses a machine first syncs its folder:
//
//	store has a record, folder pending      push the folder (written here, newer)
//	store has a record, any other folder    the store wins: the folder is refreshed
//	store has none, folder has a manifest   push it (no marker, or the store lost it)
//	store fails                             the folder is used as it is, silently
//
// In local mode none of this runs.

func machineDigest(r MachineRecord) string {
	return digestOf(string(r.Kind), r.Address, r.Username, r.Password, r.PrivateKey, r.PublicKey)
}

// hasMachineCreds is false for a record that is only a manifest: there is
// nothing to hand to the store (a store record needs a login).
func hasMachineCreds(r MachineRecord) bool {
	return r.Username != "" || r.Password != "" || r.PrivateKey != "" || r.PublicKey != ""
}

func (s *Service) machineLostLine(name string) string {
	return fmt.Sprintf("%s: the store could not be reached; credentials for machine %s are only on this machine (%s/)",
		s.Store.Host(), name, vmtool.MachineDir(name))
}

func writeMachineCache(ctx context.Context, rec MachineRecord) error {
	if err := (LocalCredentials{}).PutMachine(ctx, rec); err != nil {
		return err
	}
	return vmtool.WriteSourceMarker(vmtool.MachineDir(rec.Name), vmtool.SourceMarker{State: vmtool.SourceStore})
}

// pushMachine PUTs the cached record to the store: marker store and (with a
// notice) one line on success; marker pending and the one "only here" line on
// failure.
func (s *Service) pushMachine(ctx context.Context, local MachineRecord, notice string, note noteFunc) {
	dir := vmtool.MachineDir(local.Name)
	if !hasMachineCreds(local) {
		return
	}
	if err := s.Store.CredentialsFor(ctx).PutMachine(ctx, local); err != nil {
		_ = vmtool.WriteSourceMarker(dir, vmtool.SourceMarker{State: vmtool.SourcePending})
		note(s.machineLostLine(local.Name))
		return
	}
	_ = vmtool.WriteSourceMarker(dir, vmtool.SourceMarker{State: vmtool.SourceStore})
	if notice != "" {
		note(notice)
	}
}

// syncMachine reconciles .machines/<name>/ with the store's record, as above.
func (s *Service) syncMachine(ctx context.Context, name string, note noteFunc) {
	if !s.storeMode(ctx) || !vmtool.ValidMachineName(name) {
		return
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	src := s.Store.CredentialsFor(ctx)
	local, lerr := LocalCredentials{}.GetMachine(ctx, name)
	mk, hasMk, _ := vmtool.ReadSourceMarker(vmtool.MachineDir(name))
	rec, err := src.GetMachine(cctx, name)
	switch {
	case err == nil:
		if lerr == nil && hasMk && mk.State == vmtool.SourcePending {
			s.pushMachine(cctx, *local, fmt.Sprintf("credentials for machine %s pushed to the store at %s", name, s.Store.Host()), note)
			return
		}
		if lerr != nil || machineDigest(*rec) != machineDigest(*local) || !hasMk || mk.State != vmtool.SourceStore {
			_ = writeMachineCache(ctx, *rec)
		}
	case vmtool.IsNotFound(err):
		if lerr == nil {
			s.pushMachine(cctx, *local, fmt.Sprintf("credentials for machine %s pushed to the store at %s", name, s.Store.Host()), note)
		}
	}
}

// machineSnapshot is the digest of the machine's cached record, taken before a
// playbook runs; "" when there is none.
func machineSnapshot(ctx context.Context, name string) string {
	r, err := LocalCredentials{}.GetMachine(ctx, name)
	if err != nil {
		return ""
	}
	return machineDigest(*r)
}

// handOffMachine pushes the machine's folder after a playbook run (a failed run
// as well as a successful one) when it changed, or when it is not yet marked as
// a cache of the store's record.
func (s *Service) handOffMachine(ctx context.Context, name, before string, note noteFunc) {
	if !s.storeMode(ctx) {
		return
	}
	now, err := LocalCredentials{}.GetMachine(ctx, name)
	if err != nil {
		return
	}
	if mk, ok, _ := vmtool.ReadSourceMarker(vmtool.MachineDir(name)); ok && mk.State == vmtool.SourceStore && machineDigest(*now) == before {
		return
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	s.pushMachine(cctx, *now, "", note)
}

// loadMachineFor is loadMachine after syncing the folder with the store.
func (s *Service) loadMachineFor(ctx context.Context, op, name string) (*vmtool.Machine, error) {
	if s.storeMode(ctx) {
		s.syncMachine(ctx, name, s.noter(nil))
	}
	return loadMachine(op, name)
}
