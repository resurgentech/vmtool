package remote

// fetchImage client: the daemon downloads a base image from the store
// itself and streams NDJSON progress. The three ways a daemon "cannot fetch"
// are told apart by *FetchError.Kind (see FetchFailure), so a caller (the
// relay of tranche 10, S11) can fall back to uploading the image itself.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
)

const (
	// NoStorePrefix begins the daemon's 409 when its config.yml has no
	// store_url: "no store configured on <host>; redeploy with a store".
	NoStorePrefix = "no store configured on "
	// StoreUnreachablePrefix begins the `error` line's message when the
	// daemon could not reach the store: "store unreachable from <host>: ...".
	StoreUnreachablePrefix = "store unreachable from "
	// maxFetchLine bounds one NDJSON line.
	maxFetchLine = 64 << 10
)

// FetchFailure says why a fetch did not happen or did not finish.
type FetchFailure int

const (
	// FetchOther is any failure not named below (a full disk, a dropped
	// connection mid-body, a bad stream).
	FetchOther FetchFailure = iota
	// FetchNoStore: the daemon has no store_url (HTTP 409 with NoStorePrefix).
	// Relay-able: redeploy would fix it, upload works now.
	FetchNoStore
	// FetchDaemonTooOld: the daemon predates fetchImage (HTTP 404 with a
	// plain-text, non-JSON body). Relay-able.
	FetchDaemonTooOld
	// FetchStoreUnreachable: the daemon could not reach the store (an `error`
	// line with reason store_unreachable). Relay-able.
	FetchStoreUnreachable
	// FetchStoreRefused: the store answered but not with the image (an
	// expired or spent ticket is 401). Not a reason to relay by itself.
	FetchStoreRefused
	// FetchMismatch: the digest or size was not the one asked for. Never a
	// reason to relay: create fails.
	FetchMismatch
)

func (k FetchFailure) String() string {
	return [...]string{"other", "no store configured", "daemon too old", "store unreachable", "store refused", "mismatch"}[k]
}

// Relayable reports whether the client may answer this failure by streaming
// the image to the daemon's uploadImage itself.
func (k FetchFailure) Relayable() bool {
	return k == FetchNoStore || k == FetchDaemonTooOld || k == FetchStoreUnreachable
}

// FetchError is a fetch that failed in one of the ways FetchFailure names.
// It travels inside an *app.Error (errors.As finds it). Its message is the
// daemon's, which never holds the ticket.
type FetchError struct {
	Kind FetchFailure
	Host string
	Msg  string
}

func (e *FetchError) Error() string {
	if e.Msg != "" {
		return e.Host + ": " + e.Msg
	}
	return fmt.Sprintf("%s: fetch failed (%s)", e.Host, e.Kind)
}

// IsFetchUnsupported reports whether err is a daemon that predates fetchImage.
func IsFetchUnsupported(err error) bool {
	var fe *FetchError
	return errors.As(err, &fe) && fe.Kind == FetchDaemonTooOld
}

// FetchImage asks the daemon to download name from its store with ticket
// (fetchImage) and calls on with every NDJSON line, in order, until the done
// or error line. It returns nil after done. An error line is returned as a
// *FetchError after on has seen it; a refusal before the stream (HTTP
// status) is an ordinary StatusError, except 409 "no store configured" and
// the plain 404 of an older daemon, which are *FetchError too. The ticket is
// in the request body only and in no error.
func (m *Manager) FetchImage(ctx context.Context, name string, req api.FetchImageRequest, on func(api.FetchImageEvent)) error {
	if req.Size <= 0 {
		return fail(app.KindInvalid, fmt.Errorf("%s: fetch %s: size must be > 0", m.name, name))
	}
	ctx, cancel := context.WithTimeout(ctx, m.uploadTimeout(req.Size))
	defer cancel()
	resp, err := m.client.FetchImage(ctx, m.name, name, req)
	if err != nil {
		return m.transportErr("fetchImage", m.uploadTimeout(req.Size), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return m.transportErr("fetchImage", m.uploadTimeout(req.Size), err)
		}
		return m.fetchStatusErr(resp.StatusCode, body)
	}
	br := bufio.NewReaderSize(resp.Body, 4096)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			var ev api.FetchImageEvent
			if jerr := json.Unmarshal(line, &ev); jerr != nil {
				return fail(app.KindBadGateway, fmt.Errorf("%s: fetchImage: a line of the stream is not JSON", m.name))
			}
			if ev.Error != nil {
				// The caller sees the event too: clean its text first.
				ev.Error = ptr(m.clean(*ev.Error))
			}
			if on != nil {
				on(ev)
			}
			switch ev.Event {
			case api.FetchImageEventEventDone:
				return nil
			case api.FetchImageEventEventError:
				return m.fetchLineErr(ev)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fail(app.KindBadGateway, fmt.Errorf("%s: fetchImage: the stream ended without a done or error line", m.name))
			}
			return m.transportErr("fetchImage", m.uploadTimeout(req.Size), err)
		}
	}
}

// readLine reads one line (without its newline), at most maxFetchLine bytes.
func readLine(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		out = append(out, chunk...)
		if len(out) > maxFetchLine {
			return nil, fmt.Errorf("stream line longer than %d bytes", maxFetchLine)
		}
		if err != nil || !isPrefix {
			return out, err
		}
	}
}

func (m *Manager) fetchStatusErr(status int, body []byte) error {
	var e api.Error
	jsonErr := json.Unmarshal(body, &e) != nil
	switch {
	case status == http.StatusNotFound && jsonErr:
		// An older daemon has no such route: Go's mux answers plain text.
		return fail(app.KindNotFound, &FetchError{Kind: FetchDaemonTooOld, Host: m.name,
			Msg: "the daemon does not support fetchImage (it predates API 2.3.0)"})
	case status == http.StatusConflict && !jsonErr && strings.HasPrefix(e.Error, NoStorePrefix):
		return fail(app.KindConflict, &FetchError{Kind: FetchNoStore, Host: m.name, Msg: m.clean(e.Error)})
	}
	return m.statusErr(status, body)
}

func (m *Manager) fetchLineErr(ev api.FetchImageEvent) error {
	k := FetchOther
	if ev.Reason != nil {
		switch *ev.Reason {
		case api.StoreUnreachable:
			k = FetchStoreUnreachable
		case api.StoreRefused:
			k = FetchStoreRefused
		case api.Mismatch:
			k = FetchMismatch
		}
	}
	return fail(app.KindBadGateway, &FetchError{Kind: k, Host: m.name, Msg: m.clean(derefString(ev.Error))})
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

var _ app.ImageFetcher = (*Manager)(nil)

// Relayable makes *FetchError an app.RelayableFetch.
func (e *FetchError) Relayable() bool { return e.Kind.Relayable() }

// RelayReason is app.RelayReasoner: why the host cannot fetch, for the
// relay's one line.
func (e *FetchError) RelayReason() string {
	switch e.Kind {
	case FetchNoStore:
		return "no store configured on the host"
	case FetchDaemonTooOld:
		return "the host's daemon predates fetchImage"
	case FetchStoreUnreachable:
		return "the store is unreachable from the host"
	}
	return ""
}

// FetchFromStore is app.ImageFetcher: FetchImage with app's types. The
// ticket goes into the request body and nowhere else.
func (m *Manager) FetchFromStore(ctx context.Context, name, ticket, sha256 string, size int64, progress func(received, total int64)) error {
	return m.FetchImage(ctx, name, api.FetchImageRequest{Sha256: sha256, Size: size, Ticket: ticket}, func(ev api.FetchImageEvent) {
		if progress != nil && ev.Event == api.FetchImageEventEventProgress && ev.Received != nil {
			total := int64(0)
			if ev.Total != nil {
				total = *ev.Total
			}
			progress(*ev.Received, total)
		}
	})
}
