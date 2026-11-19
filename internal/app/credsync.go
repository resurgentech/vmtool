package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// This file is the client's policy for guest credentials in store mode: when
// the cache folder .machines/<vm>.<host>.vmtool/ is fetched, pushed, refreshed
// or moved aside. The primitives (marker, atomic writes, stale move) are in
// pkg/vmtool/credcache.go; the store's calls are CredentialSource.
//
// In local mode (no server:, or the store unreachable at the start of the
// command) none of this runs: the folder is the record, no marker is written,
// and every function here returns at its first line.
//
// Nothing here puts a password, a key or the store token in a notice, an
// error or a progress event: lines name the VM, the store's host and the
// folder, nothing else.

// CredTimeout bounds each store credential call made after the mode was
// decided. A var so tests can shorten it.
var CredTimeout = 10 * time.Second

// storeMode reports whether this command is in store mode (deciding it on the
// first call).
func (s *Service) storeMode(ctx context.Context) bool {
	return s.Store != nil && s.Store.Mode(ctx) == ModeStore
}

func (s *Service) credCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, CredTimeout)
}

// noteFunc delivers one notice line.
type noteFunc func(line string)

// noter delivers a notice through the store session (stderr, or the warnings
// header), once per distinct line, and, when it was delivered, also as a
// progress event (stage credentials, status notice) so a streamed response
// whose headers are already written still carries it. progress may be nil.
func (s *Service) noter(progress ProgressFunc) noteFunc {
	return func(line string) {
		if s.Store.Notify(line) {
			progress.emit("credentials", "notice", line, "")
		}
	}
}

func (s *Service) lostLine(ref VMRef) string {
	return fmt.Sprintf("%s: the store could not be reached; credentials for %s are only on this machine (%s/)",
		s.Store.Host(), ref, vmtool.CredsDir(ref.Host, ref.Name))
}

func (s *Service) redeployLine(ref VMRef) string {
	return fmt.Sprintf("%s reports no VM uuid (a daemon older than 2.3.0): credentials for %s are not synced with the store; redeploy %s",
		ref.Host, ref, ref.Host)
}

// digestOf is a fingerprint of a credential's values, "" for an empty one. A
// folder and a store record with the same values have the same digest, so
// "the folder changed" does not depend on how a file was formatted.
func digestOf(fields ...string) string {
	h := sha256.New()
	nonEmpty := false
	for _, f := range fields {
		if f != "" {
			nonEmpty = true
		}
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	if !nonEmpty {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func vmDigest(c VMCredential) string {
	return digestOf(c.Username, c.Password, c.PrivateKey, c.PublicKey)
}

// folderDigest is the digest of the credentials in dir, "" when there are none.
func folderDigest(dir string) string {
	c, ok, err := readCredDir(dir)
	if err != nil || !ok {
		return ""
	}
	return vmDigest(c)
}

func vmFolder(ref VMRef) string { return vmtool.CredsDir(ref.Host, ref.Name) }

// writeVMCache writes the store's record into the cache folder, marked as a
// cache of the store's record for uuid.
func writeVMCache(ref VMRef, uuid string, rec VMCredential) error {
	dir := vmFolder(ref)
	if err := writeCredDir(dir, rec); err != nil {
		return err
	}
	return vmtool.WriteSourceMarker(dir, vmtool.SourceMarker{State: vmtool.SourceStore, UUID: uuid})
}

// PrepareVMCreds runs the reconciliation for VM ref before a command uses its
// credentials (ssh, exec, playbook, console, resize-disk): in store mode it
// compares the cache folder with the domain's current UUID and the store's
// record. It is a no-op in local mode, with no network call and no Info. A
// store that fails here is not an error: reads fall back to the cache.
func (s *Service) PrepareVMCreds(ctx context.Context, m Manager, ref VMRef) error {
	if !s.storeMode(ctx) {
		return nil
	}
	info, err := m.Info(ref.Name)
	if err != nil {
		return wrap("credentials", err)
	}
	s.prepareCreds(ctx, ref, info.UUID, s.noter(nil))
	return nil
}

// prepareCreds is the reconciliation, with the UUID already known (""
// from a daemon older than 2.3.0).
//
//	folder missing                       fetch (a stale record is not used)
//	marker names another UUID            stale: moved aside, never pushed; then fetch
//	pending, current UUID                push
//	store, current UUID                  check the record: lost or stale -> push back; changed -> refresh
//	no marker / pending, UUID unknown    record current: the store wins; none: push; for another UUID: stale
func (s *Service) prepareCreds(ctx context.Context, ref VMRef, uuid string, note noteFunc) {
	if !s.storeMode(ctx) {
		return
	}
	if uuid == "" {
		note(s.redeployLine(ref))
		return
	}
	dir := vmFolder(ref)
	mk, hasMk, _ := vmtool.ReadSourceMarker(dir)
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	src := s.Store.CredentialsFor(ctx)

	switch {
	case !vmtool.HasVMCreds(ref.Host, ref.Name):
		s.fetchCache(cctx, ref, uuid)
	case hasMk && mk.UUID != "" && mk.UUID != uuid:
		s.moveStale(ref, note)
		s.fetchCache(cctx, ref, uuid)
	case hasMk && mk.State == vmtool.SourcePending && mk.UUID == uuid:
		s.pushFolder(cctx, ref, uuid, fmt.Sprintf("credentials for %s pushed to the store at %s", ref, s.Store.Host()), note)
	case hasMk && mk.State == vmtool.SourceStore:
		rec, err := src.GetVM(cctx, ref.Host, ref.Name, uuid)
		switch {
		case err == nil:
			if vmDigest(*rec) != folderDigest(dir) {
				_ = writeVMCache(ref, uuid, *rec)
			}
		case vmtool.IsNotFound(err): // lost (or, when stale, replaced): this folder is current
			s.pushFolder(cctx, ref, uuid, fmt.Sprintf("the store at %s had lost its record for %s: credentials pushed back", s.Store.Host(), ref), note)
		}
	default:
		rec, err := src.GetVM(cctx, ref.Host, ref.Name, uuid)
		switch {
		case err == nil:
			_ = writeVMCache(ref, uuid, *rec)
		case errors.Is(err, ErrStaleCredential):
			s.moveStale(ref, note)
		case vmtool.IsNotFound(err):
			s.pushFolder(cctx, ref, uuid, fmt.Sprintf("credentials for %s pushed to the store at %s", ref, s.Store.Host()), note)
		}
	}
}

// fetchCache copies the store's record for uuid into the cache. A failure, a
// missing record and a stale one all leave the cache as it was (a record for
// another UUID is never used).
func (s *Service) fetchCache(ctx context.Context, ref VMRef, uuid string) error {
	rec, err := s.Store.CredentialsFor(ctx).GetVM(ctx, ref.Host, ref.Name, uuid)
	if err != nil {
		return err
	}
	return writeVMCache(ref, uuid, *rec)
}

// moveStale sets aside a folder that belongs to a VM that no longer exists:
// .machines/.stale/<folder>-<date>/. It is never deleted and never pushed.
func (s *Service) moveStale(ref VMRef, note noteFunc) {
	dest, err := vmtool.MoveStale(vmFolder(ref), time.Now())
	if err != nil {
		note(fmt.Sprintf("credentials in %s/ are stale (the VM was recreated) and could not be moved aside: %v", vmFolder(ref), err))
		return
	}
	note(fmt.Sprintf("credentials in %s/ were for a VM that was deleted and recreated (stale): moved to %s/, not pushed", vmFolder(ref), dest))
}

// pushFolder PUTs the cache folder's credentials to the store as the record for
// uuid. On success the marker becomes store, and notice (if not empty) is the
// one line. On failure the marker becomes pending and the one line says the
// store could not be reached and the credentials are only here.
func (s *Service) pushFolder(ctx context.Context, ref VMRef, uuid, notice string, note noteFunc) {
	dir := vmFolder(ref)
	c, ok, err := readCredDir(dir)
	if err != nil || !ok {
		return
	}
	c.UUID = uuid
	if err := s.Store.CredentialsFor(ctx).PutVM(ctx, ref.Host, ref.Name, c); err != nil {
		_ = vmtool.WriteSourceMarker(dir, vmtool.SourceMarker{State: vmtool.SourcePending, UUID: uuid})
		note(s.lostLine(ref))
		return
	}
	_ = vmtool.WriteSourceMarker(dir, vmtool.SourceMarker{State: vmtool.SourceStore, UUID: uuid})
	if notice != "" {
		note(notice)
	}
}

// credSnapshot is the digest of ref's folder, taken before a playbook runs.
func credSnapshot(ref VMRef) string { return folderDigest(vmFolder(ref)) }

// handOffCreds is the hand-off after a playbook run on a VM, whether the run
// succeeded or failed: when the folder changed since before, PUT it to the
// store. A rerun of setup_bot.yml (which keeps an existing key) therefore
// completes a hand-off that failed before. A store that fails does not fail
// the command: the folder is marked pending and one line says so.
func (s *Service) handOffCreds(ctx context.Context, ref VMRef, uuid, before string, note noteFunc) {
	if !s.storeMode(ctx) {
		return
	}
	after := credSnapshot(ref)
	if after == "" || after == before {
		return
	}
	if uuid == "" {
		note(s.redeployLine(ref))
		return
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	s.pushFolder(cctx, ref, uuid, "", note)
}

// RefetchVMCreds is the one refetch after ssh exited 255: it fetches VM ref's
// record again and reports whether the cache changed, so the caller retries
// its ssh once, and only then. A folder not in the store yet (pending) is never
// overwritten. It is false in local mode and when the store cannot answer.
func (s *Service) RefetchVMCreds(ctx context.Context, m Manager, ref VMRef) bool {
	if !s.storeMode(ctx) {
		return false
	}
	info, err := m.Info(ref.Name)
	if err != nil || info.UUID == "" {
		return false
	}
	dir := vmFolder(ref)
	if mk, ok, _ := vmtool.ReadSourceMarker(dir); ok && mk.State == vmtool.SourcePending {
		return false
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	rec, err := s.Store.CredentialsFor(ctx).GetVM(cctx, ref.Host, ref.Name, info.UUID)
	if err != nil || vmDigest(*rec) == folderDigest(dir) {
		return false
	}
	return writeVMCache(ref, info.UUID, *rec) == nil
}

// FetchVMCreds is `vmtool creds fetch host/vm`: write the store's record for
// the VM's current UUID into the cache folder, replacing what is there. It
// needs store mode.
func (s *Service) FetchVMCreds(ctx context.Context, m Manager, ref VMRef) error {
	const op = "creds_fetch"
	if err := validRef(op, ref); err != nil {
		return err
	}
	if !s.storeMode(ctx) {
		return conflict(op, fmt.Errorf("there is no store to fetch from (no server: in config.yml, or it is unreachable); .machines/ is the only copy"))
	}
	info, err := m.Info(ref.Name)
	if err != nil {
		return wrap(op, err)
	}
	if info.UUID == "" {
		return conflict(op, errors.New(s.redeployLine(ref)))
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	if err := s.fetchCache(cctx, ref, info.UUID); err != nil {
		return wrap(op, err)
	}
	return nil
}

// FetchMissingVMCreds is the tunnel's fetch: in store mode, when VM ref's cache
// folder is missing, fetch it, so the IdentityFile that ssh-config names exists
// before ssh needs it. With the folder present it returns at once, without a
// store call. uuidOf looks the domain UUID up (only when it is needed). It
// never fails the caller: the lines go through the session's notify.
func (s *Service) FetchMissingVMCreds(ctx context.Context, ref VMRef, uuidOf func(context.Context) (string, error)) {
	if s.Store == nil || validRef("credentials", ref) != nil || vmtool.HasVMCreds(ref.Host, ref.Name) {
		return
	}
	if !s.storeMode(ctx) {
		return
	}
	uuid, err := uuidOf(ctx)
	if err != nil {
		return
	}
	if uuid == "" {
		s.noter(nil)(s.redeployLine(ref))
		return
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	_ = s.fetchCache(cctx, ref, uuid)
}

// ForgetVMCreds is `vmtool creds forget host/vm`: remove the cache folder, not
// the store's record. It refuses without a store, and when the folder holds
// credentials that are not in the store (pending, or no marker): there it
// would remove the only copy.
func (s *Service) ForgetVMCreds(ctx context.Context, ref VMRef) error {
	const op = "creds_forget"
	if err := validRef(op, ref); err != nil {
		return err
	}
	if !s.storeMode(ctx) {
		return conflict(op, fmt.Errorf("there is no store: %s/ is the only copy of these credentials; delete the VM or remove the folder yourself", vmFolder(ref)))
	}
	if mk, ok, _ := vmtool.ReadSourceMarker(vmFolder(ref)); vmtool.HasVMCreds(ref.Host, ref.Name) && (!ok || mk.State != vmtool.SourceStore) {
		return conflict(op, fmt.Errorf("%s/ holds credentials that are not in the store yet (pending): remove it only after they are pushed", vmFolder(ref)))
	}
	return wrap(op, vmtool.ForgetVM(ref.Host, ref.Name))
}

// forgetCreds removes a deleted or replaced VM's credentials: the store's
// record (store mode) and the cache folder. A store that cannot be reached
// leaves its record, which is never used for the new VM (another UUID), and
// says so in one line (unless note is nil: create's replace stays quiet, so a
// store that is down costs one line per command, not two); the folder is removed either way.
func (s *Service) forgetCreds(ctx context.Context, ref VMRef, note noteFunc) error {
	if s.storeMode(ctx) {
		cctx, cancel := s.credCtx(ctx)
		defer cancel()
		if err := s.Store.CredentialsFor(ctx).DeleteVM(cctx, ref.Host, ref.Name); err != nil && note != nil {
			note(fmt.Sprintf("%s: the store could not be reached; its record for %s was not removed (a new VM of that name will not use it)", s.Store.Host(), ref))
		}
	}
	return vmtool.ForgetVM(ref.Host, ref.Name)
}
