package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/internal/vmnotes"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// A VM's notes: what vmtool records about a VM besides what its host
// reports, its origin (written once, by create) and its comment. They are
// kept in the store only, keyed (host, libvirt UUID); vmtool writes nothing
// of its own into a VM's libvirt definition. With no store in use there are
// none: create records no origin and a comment is refused.

// VMNotes is a VM's notes as the store keeps them. Origin is nil when none
// was recorded; Comment is empty when there is none.
type VMNotes struct {
	UUID    string
	Origin  *vmnotes.Origin
	Comment string
}

// commentNeedsStore is the refusal of a comment with no store in use (REST
// 503); cause says why the store cannot be used.
func commentNeedsStore(op string, cause error) *Error {
	return unavailable(op, fmt.Errorf("comments are kept in the store, and no store is in use (%v)", cause))
}

// notesClient is the store's client for reading notes without deciding the
// mode: nil for no store, an unusable token, or a mode already decided local;
// otherwise the client, also while the mode is undecided. Nothing is printed
// for a store that does not answer: the caller shows no notes.
func (s *StoreSession) notesClient() *storeclient.Client {
	if s == nil || s.tokenErr != nil || s.client == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decided && s.mode != ModeStore {
		return nil
	}
	return s.client
}

// SetComment sets the comment of ref, or clears it when comment is empty, in
// the store. The VM's host is not asked (it may sleep): the store needs a
// live inventory row of (host, name). A comment that breaks the rules is
// KindInvalid and the store is not called; no store in use is
// KindUnavailable; no row is KindNotFound; a gone row is KindConflict.
func (s *Service) SetComment(ctx context.Context, ref VMRef, comment string) (*VMNotes, error) {
	if err := validRef("comment", ref); err != nil {
		return nil, err
	}
	if err := vmnotes.ValidateComment(comment); err != nil {
		return nil, invalid("comment", err)
	}
	c, err := s.Store.requireStore(ctx)
	if err != nil {
		return nil, commentNeedsStore("comment", err)
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	n, err := c.PutVMNotes(cctx, ref.Host, ref.Name, storeclient.VMNotesInput{Comment: &comment})
	switch {
	case errors.Is(err, storeclient.ErrNotFound):
		return nil, &Error{Kind: KindNotFound, Op: "comment", Err: fmt.Errorf("%s: the store's inventory has no VM %s on %s (its host has not reported it)", ref, ref.Name, ref.Host)}
	case errors.Is(err, storeclient.ErrNotesRefused):
		return nil, conflict("comment", fmt.Errorf("%s: its host no longer has it (the store's inventory row is gone); its comment cannot be changed", ref))
	case err != nil:
		return nil, unavailable("comment", fmt.Errorf("%s: the store at %s could not set the comment: %v", ref, s.Store.Host(), err))
	}
	return notesFromAPI(n.Uuid, n.Origin, n.Comment), nil
}

// Notes reads ref's notes from the store for `vmtool info`: one read of the
// host's inventory, matched by name and, when uuid is given, by UUID. nil
// when the store is not read (local mode) or fails, with no line printed; a
// VM the store has no notes for answers empty notes.
func (s *Service) Notes(ctx context.Context, ref VMRef, uuid string) *VMNotes {
	c := s.Store.notesClient()
	if c == nil {
		return nil
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	inv, err := c.Inventory(cctx, ref.Host, false)
	if err != nil {
		return nil
	}
	for _, v := range inv.Vms {
		if v.Host == ref.Host && v.Name == ref.Name && (uuid == "" || strings.EqualFold(v.Uuid, uuid)) {
			return notesFromAPI(v.Uuid, v.Origin, v.Comment)
		}
	}
	return &VMNotes{}
}

// AllNotes reads every live VM's notes from the store in one read, for the
// TUI. nil when the store is not read or fails.
func (s *Service) AllNotes(ctx context.Context) map[VMRef]VMNotes {
	c := s.Store.notesClient()
	if c == nil {
		return nil
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	inv, err := c.Inventory(cctx, "", false)
	if err != nil {
		return nil
	}
	out := make(map[VMRef]VMNotes, len(inv.Vms))
	for _, v := range inv.Vms {
		out[VMRef{Host: v.Host, Name: v.Name}] = *notesFromAPI(v.Uuid, v.Origin, v.Comment)
	}
	return out
}

func notesFromAPI(uuid string, o *storeclient.VMOrigin, comment string) *VMNotes {
	n := &VMNotes{UUID: uuid, Comment: comment}
	if o != nil {
		d := func(p *string) string {
			if p == nil {
				return ""
			}
			return *p
		}
		n.Origin = &vmnotes.Origin{Image: d(o.Image), ImageSHA256: d(o.ImageSha256), Template: d(o.Template),
			CloneKind: d(o.Clone), Pool: d(o.Pool)}
		if t, err := time.Parse(time.RFC3339, d(o.Created)); err == nil {
			n.Origin.Created = t.UTC()
		}
	}
	return n
}

func originToStore(o *vmnotes.Origin) *storeclient.VMOrigin {
	opt := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	out := &storeclient.VMOrigin{Image: opt(o.Image), ImageSha256: opt(o.ImageSHA256), Template: opt(o.Template),
		Clone: opt(o.CloneKind), Pool: opt(o.Pool)}
	if !o.Created.IsZero() {
		out.Created = opt(o.Created.UTC().Format(time.RFC3339))
	}
	return out
}

// createNotes is what a create knows, before define, of the VM's origin.
type createNotes struct {
	in      CreateVMInput
	ref     VMRef
	pools   []vmtool.PoolInfo // nil when the create did not list them
	cloned  string            // the boot disk's path
	sha     string            // the image's digest on the host, when known
	created time.Time
}

// recordNotes writes the new VM's origin, and its comment if one was given,
// to the store in one call, keyed by the UUID the host gave the domain. It
// runs once the domain exists and its UUID is known. With no store in use it
// does nothing and prints nothing. If the store fails, the create still
// succeeds: one line says the notes were not saved and how to set the
// comment later.
func (s *Service) recordNotes(ctx context.Context, m Manager, n createNotes, uuid string) {
	if uuid == "" || !s.storeMode(ctx) {
		return
	}
	pools := n.pools
	if pools == nil && !n.in.Noclone && strings.HasPrefix(n.cloned, "/dev/") {
		// A /dev path may be a thin snapshot or a thick LV: only the pool list
		// tells (the create skipped it when a disk cache was given).
		pools, _ = m.ListPools()
	}
	o := originOf(n.in, pools, n.cloned, n.sha, n.created)
	if o.CloneKind == vmnotes.CloneKindThin {
		thinTemplate(m, o)
	}
	in := storeclient.VMNotesInput{Uuid: &uuid, Origin: originToStore(o)}
	if n.in.Comment != "" {
		c := n.in.Comment
		in.Comment = &c
	}
	cctx, cancel := s.credCtx(ctx)
	defer cancel()
	_, err := s.Store.client.PutVMNotes(cctx, n.ref.Host, n.ref.Name, in)
	if err == nil {
		return
	}
	what := "its origin was"
	if n.in.Comment != "" {
		what = "its origin and comment were"
	}
	s.noter(n.in.OnProgress)(fmt.Sprintf("%s: the store did not take the notes of %s (%v): %s not saved; set the comment later with 'vmtool comment %s TEXT'",
		s.Store.Host(), n.ref, err, what, n.ref))
}

// thinTemplate names the template a thin snapshot was made from, as the
// host's thin pool chose it (vmtool.FindTemplate: the image template of that
// image and digest, else the saved template of that name), and takes the
// template's digest when the create did not know the image's. Best effort:
// a host that cannot list its templates leaves the origin as it is.
func thinTemplate(m Manager, o *vmnotes.Origin) {
	tm, ok := templateManager(m)
	if !ok {
		return
	}
	ts, err := tm.ListTemplates()
	if err != nil {
		return
	}
	var inPool []vmtool.Template
	for _, t := range ts {
		if o.Pool == "" || t.Pool == o.Pool {
			inPool = append(inPool, t.Template)
		}
	}
	if t, ok := vmtool.FindTemplate(inPool, o.Image, o.ImageSHA256); ok {
		o.Template = t.Name
		if o.ImageSHA256 == "" {
			o.ImageSHA256 = t.SHA256
		}
	}
}
