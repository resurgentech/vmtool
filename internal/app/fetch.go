package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// ImageFetcher is implemented by a Manager whose host can download a base
// image from the store itself (remote.Manager, fetchImage): the daemon gets a
// one-use ticket and the client reads no image bytes. It is separate from
// ImageStore so no existing ImageStore fake has to change.
type ImageFetcher interface {
	// FetchFromStore asks the host to download name from its store with
	// ticket and checks it against sha256 and size. progress, if set, is
	// called with the bytes the host has received so far and the total. A
	// failure the client may answer by relaying the image itself (S11) is an
	// error whose chain holds a RelayableFetch.
	//
	// The ticket is a credential: an implementation sends it in the request
	// body to the host and puts it in no error, event or log line.
	FetchFromStore(ctx context.Context, name, ticket, sha256 string, size int64, progress func(received, total int64)) error
}

// RelayableFetch is implemented by the error of a fetch the host could not do
// but the client may answer by streaming the image to the host (no store_url
// on the daemon, a daemon that predates fetchImage, a store the daemon cannot
// reach). It is S11's hook.
type RelayableFetch interface {
	error
	Relayable() bool
}

// RelayReasoner is implemented by a relayable fetch error that can say, in a
// few words, why the host cannot fetch ("no store configured on the host").
// It is the reason in the one line the relay prints.
type RelayReasoner interface{ RelayReason() string }

const defaultRelayReason = "the host cannot fetch it from the store"

func relayReason(err error) string {
	var r RelayReasoner
	if errors.As(err, &r) {
		if s := r.RelayReason(); s != "" {
			return s
		}
	}
	return defaultRelayReason
}

// IsRelayableFetch reports whether err is a fetch failure a relay can answer.
func IsRelayableFetch(err error) bool {
	var r RelayableFetch
	return errors.As(err, &r) && r.Relayable()
}

// StoreImageHint says how to put a local image into the store.
const StoreImageHint = "vmtool images push <file> --name %s"

// scrub removes ticket from msg. Nothing here should hold it; this is the
// belt for the braces.
func scrub(msg, ticket string) string {
	if ticket == "" {
		return msg
	}
	return strings.ReplaceAll(msg, ticket, "[ticket]")
}

// fetchViaStore makes the host hold the store's copy of in.Image without the
// client reading any image bytes. handled is true when nothing more is to be
// done (the host is current, or the fetch ran, or it failed with err). When
// handled is false the store cannot serve the image (it lacks it, or failed
// before a fetch could start) and the caller goes on to this machine's own
// copy; lacks says the store has no such image.
func (s *Service) fetchViaStore(ctx context.Context, st ImageStore, f ImageFetcher, rem RemoteImage, in CreateVMInput) (handled, lacks bool, err error) {
	cat := s.Store.CatalogFor(ctx)
	img, gerr := cat.Get(ctx, in.Image)
	if gerr != nil {
		if vmtool.IsNotFound(gerr) {
			return false, true, nil
		}
		s.noteUpload(in, fmt.Sprintf("%s: the store could not be asked for %s (%v); using this machine's copy", s.Store.Host(), in.Image, gerr))
		return false, false, nil
	}
	if rem.SHA256 == img.SHA256 && rem.Size == img.Size {
		emit(in, "upload", "done", fmt.Sprintf("%s is up to date on %s", in.Image, in.Host), "")
		return true, false, nil
	}
	t, terr := cat.Ticket(ctx, in.Image)
	if terr != nil {
		if vmtool.IsNotFound(terr) {
			return false, true, nil
		}
		s.noteUpload(in, fmt.Sprintf("%s: the store would not give a download ticket for %s (%v); using this machine's copy", s.Store.Host(), in.Image, terr))
		return false, false, nil
	}
	ticket := t.Ticket
	total := t.Size
	emit(in, "upload", "start", fmt.Sprintf("%s fetches %s from the store (%d bytes)", in.Host, in.Image, total), "")
	step, next := uploadStep(total), int64(0)
	progress := func(received, tot int64) {
		if tot <= 0 {
			tot = total
		}
		if received >= next || received == tot {
			next = received + step
			in.OnProgress.emitEvent(ProgressEvent{Stage: "upload", Status: "progress",
				Detail: fmt.Sprintf("%d/%d bytes", received, tot), Sent: received, Total: tot})
		}
	}
	if ferr := f.FetchFromStore(ctx, in.Image, ticket, t.SHA256, t.Size, progress); ferr != nil {
		if IsRelayableFetch(ferr) {
			// The host cannot fetch: this machine relays the store's bytes.
			return s.relayToHost(ctx, st, cat, t, in, relayReason(ferr), ticket)
		}
		msg := scrub(ferr.Error(), ticket)
		emit(in, "upload", "error", msg, "")
		kind := KindBadGateway
		var ae *Error
		if errors.As(ferr, &ae) {
			kind = ae.Kind
		}
		return true, false, &Error{Kind: kind, Op: "create", Err: &scrubbed{msg: msg, err: ferr}}
	}
	emit(in, "upload", "done", fmt.Sprintf("%s fetched %s from the store", in.Host, in.Image), "")
	return true, false, nil
}

// relayToHost streams the store's copy of in.Image (this machine's own store
// token) into the host's uploadImage with the store's digest and size: no
// temp file, no local file. t is the store's metadata for the image. If the
// relay cannot be done and this machine has a copy, handled is false and the
// caller uploads that copy as in local mode, after a line saying so; with no
// copy the create fails.
func (s *Service) relayToHost(ctx context.Context, st ImageStore, cat ImageCatalog, t *ImageTicket, in CreateVMInput, reason, ticket string) (handled, lacks bool, err error) {
	s.noteUpload(in, fmt.Sprintf("relaying %s to %s through this machine: %s", in.Image, in.Host, reason))
	rerr := s.relay(ctx, st, cat, t, in)
	if rerr == nil {
		return true, false, nil
	}
	msg := scrub(rerr.Error(), ticket)
	if s.Images != nil {
		if _, ferr := s.Images.FindImage(in.Image); ferr == nil {
			s.noteUpload(in, fmt.Sprintf("relaying %s to %s failed (%s); using this machine's copy", in.Image, in.Host, msg))
			return false, false, nil
		}
	}
	msg = fmt.Sprintf("relaying %s to %s failed (%s) and this machine has no copy of it", in.Image, in.Host, msg)
	emit(in, "upload", "error", msg, "")
	kind := KindBadGateway
	var ae *Error
	if errors.As(rerr, &ae) {
		kind = ae.Kind
	}
	return true, false, &Error{Kind: kind, Op: "create", Err: errors.New(msg)}
}

func (s *Service) relay(ctx context.Context, st ImageStore, cat ImageCatalog, t *ImageTicket, in CreateVMInput) error {
	body, got, err := cat.Open(ctx, in.Image)
	if err != nil {
		return fmt.Errorf("opening the store's copy: %w", err)
	}
	defer body.Close()
	// The digest and size the upload is checked against are the store's
	// metadata; the blob must say the same or nothing is uploaded.
	if got.SHA256 != t.SHA256 || (got.Size > 0 && got.Size != t.Size) {
		return fmt.Errorf("the store's copy does not match its own metadata (sha256 %q size %d, expected %q size %d)", got.SHA256, got.Size, t.SHA256, t.Size)
	}
	total := t.Size
	emit(in, "upload", "start", fmt.Sprintf("relaying %s to %s (%d bytes)", in.Image, in.Host, total), "")
	step, next := uploadStep(total), int64(0)
	progress := func(sent int64) {
		if sent >= next || sent == total {
			next = sent + step
			in.OnProgress.emitEvent(ProgressEvent{Stage: "upload", Status: "progress",
				Detail: fmt.Sprintf("%d/%d bytes", sent, total), Sent: sent, Total: total})
		}
	}
	if err := st.UploadImage(ctx, in.Image, body, total, t.SHA256, progress); err != nil {
		return err
	}
	emit(in, "upload", "done", fmt.Sprintf("relayed %s to %s", in.Image, in.Host), "")
	return nil
}

// noteUpload sends one line to the command's user (once) and, when it went
// out, as a progress event (stage upload, status notice).
func (s *Service) noteUpload(in CreateVMInput, line string) {
	if s.Store.Notify(line) {
		emit(in, "upload", "notice", line, "")
	}
}

// scrubbed is an error with a message cleaned of the ticket that still
// unwraps to the original, so errors.As finds the fetch's own type (S11).
type scrubbed struct {
	msg string
	err error
}

func (e *scrubbed) Error() string { return e.msg }
func (e *scrubbed) Unwrap() error { return e.err }
