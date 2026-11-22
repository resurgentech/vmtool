package server

// fetchImage: the hypervisor's daemon downloads a base image from the store
// and streams NDJSON progress (spec/openapi/vmtool.yaml, API 2.3.0).
//
//   - The daemon dials one address: the `store_url` of its own config.yml
//     (handlers.fetchURL), plus the fixed path /v1/images/<name>/blob. Nothing
//     in the request (body, header, path beyond a validated image name) changes
//     scheme, host, port or path prefix. https only, system trust store
//     (handlers.fetchRoots is a test seam, never a request field), no proxy
//     (an environment proxy would otherwise see where the daemon goes and
//     carry the credential), no redirect, no HTTP/2, no compression.
//   - The request's one-use ticket goes into `Authorization: Ticket <t>` of
//     that one request and nowhere else: not a log line, not an error, not an
//     NDJSON line, not a response. Every message built here passes through
//     scrubTicket as well, so a transport error that happened to quote it
//     could not carry it out. The request body is never logged.
//   - Refusals that need no download are plain JSON statuses (the same as
//     uploadImage's: foreign 409, one at a time 409, bad request 400, no
//     store_url 409). From the 200 on, every outcome is an NDJSON line:
//     progress*, then one done or one error with a reason (see
//     api.FetchImageEventReason; remote.FetchFailure is its client side).
//   - The bytes go through (*receipt).receive, the receive path uploadImage
//     uses: temporary file, SHA-256 and size checked, hard link, manifest entry
//     only on a match.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/remote"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

const (
	// fetchDialTimeout bounds connecting to the store (TCP and TLS).
	fetchDialTimeout = 10 * time.Second
	// fetchHeaderTimeout bounds the wait for the store's response headers.
	fetchHeaderTimeout = 30 * time.Second
	// fetchIdle is how long a download may deliver nothing before it fails.
	fetchIdle = uploadIdle
	// progressMaxStep is the most bytes between two progress lines.
	progressMaxStep = 64 << 20
)

// ticketRE is the shape of the store's tickets (base64url) with slack: it
// keeps whitespace, control characters and header separators out of the one
// header the ticket goes into.
var ticketRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// scrubTicket removes the ticket from s.
func scrubTicket(s, ticket string) string {
	if ticket == "" {
		return s
	}
	return strings.ReplaceAll(s, ticket, "[ticket]")
}

// noStoreMsg is the 409 for a daemon whose config.yml has no store_url.
func noStoreMsg(host string) string {
	return fmt.Sprintf("%s%s; redeploy with a store", remote.NoStorePrefix, host)
}

// imageFetcher is a Manager whose host's daemon can fetch (remote.Manager).
type imageFetcher interface {
	FetchImage(ctx context.Context, name string, req api.FetchImageRequest, on func(api.FetchImageEvent)) error
}

// --- routing ---

// FetchImage on the strict interface is never reached: tunnelMux serves the
// route itself so it can stream NDJSON.
func (h *handlers) FetchImage(_ context.Context, _ api.FetchImageRequestObject) (api.FetchImageResponseObject, error) {
	return api.FetchImage500JSONResponse{Code: statusCode(500), Error: "fetchImage streams NDJSON; served by tunnelMux"}, nil
}

func (t tunnelMux) FetchImage(w http.ResponseWriter, r *http.Request, host api.HostName, name api.ImageName) {
	setNote(r.Context(), "fetchImage", r)
	t.h.fetchImage(w, r, host, name)
}

// writeStatus answers a refusal before the stream as the operation's JSON.
func writeFetchStatus(w http.ResponseWriter, code int, msg string) {
	e := errBody(code, msg)
	switch code {
	case http.StatusBadRequest:
		_ = api.FetchImage400JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(e)}.VisitFetchImageResponse(w)
	case http.StatusForbidden:
		_ = api.FetchImage403JSONResponse(e).VisitFetchImageResponse(w)
	case http.StatusNotFound:
		_ = api.FetchImage404JSONResponse(e).VisitFetchImageResponse(w)
	case http.StatusConflict:
		_ = api.FetchImage409JSONResponse(e).VisitFetchImageResponse(w)
	case http.StatusBadGateway:
		_ = api.FetchImage502JSONResponse{HostUnreachableJSONResponse: api.HostUnreachableJSONResponse(withCode(e, api.ErrorCodeHostUnreachable))}.VisitFetchImageResponse(w)
	case http.StatusGatewayTimeout:
		_ = api.FetchImage504JSONResponse{HostTimeoutJSONResponse: api.HostTimeoutJSONResponse(e)}.VisitFetchImageResponse(w)
	default:
		_ = api.FetchImage500JSONResponse(e).VisitFetchImageResponse(w)
	}
}

// decodeFetchRequest reads and checks the body. The messages never quote a
// value of it: the ticket is a credential.
func decodeFetchRequest(r *http.Request) (api.FetchImageRequest, string) {
	var req api.FetchImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, "the request body must be JSON {\"sha256\": 64 hex digits, \"size\": bytes, \"ticket\": the store's download ticket}"
	}
	req.Sha256 = strings.ToLower(req.Sha256)
	switch {
	case !sha256Hex.MatchString(req.Sha256):
		return req, "sha256 must be 64 hex digits"
	case req.Size <= 0:
		return req, "size must be at least 1"
	case !ticketRE.MatchString(req.Ticket):
		return req, "ticket is missing or malformed (16 to 128 letters, digits, '-' and '_')"
	}
	return req, ""
}

// --- the handler ---

func (h *handlers) fetchImage(w http.ResponseWriter, r *http.Request, host, name string) {
	if h.role != config.RoleHypervisor {
		if host == h.hosts.Local() && !h.hosts.OwnListed() {
			writeFetchStatus(w, http.StatusForbidden, fmt.Sprintf(
				"fetchImage is served by hypervisor daemons, not by workstation %s; a listed host's daemon fetches", h.hosts.Local()))
			return
		}
		h.forwardFetch(w, r, host, name)
		return
	}
	if !vmtool.ValidImageName(name) {
		writeFetchStatus(w, http.StatusBadRequest, badImageName(name))
		return
	}
	req, bad := decodeFetchRequest(r)
	if bad != "" {
		writeFetchStatus(w, http.StatusBadRequest, bad)
		return
	}
	// A hypervisor serves only itself.
	if err := h.hosts.Check(host); err != nil {
		writeFetchStatus(w, http.StatusNotFound, err.Error())
		return
	}
	if h.fetchURL == "" {
		writeFetchStatus(w, http.StatusConflict, noStoreMsg(host))
		return
	}
	// Ownership and one-at-a-time, before anything is downloaded.
	rc, uerr := h.beginReceive(r.Context(), host, name, "fetch")
	if uerr != nil {
		if uerr.code >= 500 {
			slog.Error("fetchImage", "image", name, "err", scrubTicket(uerr.msg, req.Ticket))
		}
		writeFetchStatus(w, uerr.code, scrubTicket(uerr.msg, req.Ticket))
		return
	}
	defer rc.release()

	st := newNDJSON(w)
	fail := func(reason api.FetchImageEventReason, msg string) {
		msg = scrubTicket(msg, req.Ticket)
		slog.Warn("fetchImage", "image", name, "outcome", string(reason), "err", msg)
		st.send(api.FetchImageEvent{Event: api.FetchImageEventEventError, Error: &msg, Reason: &reason})
	}

	// Already cached with this digest and size: done at once.
	if e, listed, err := rc.man.Get(name); err == nil && listed && e.SHA256 == req.Sha256 && e.Size == req.Size {
		if fi, err := os.Lstat(rc.final); err == nil && fi.Mode().IsRegular() && fi.Size() == e.Size {
			slog.Info("fetchImage", "image", name, "outcome", "cached")
			st.send(api.FetchImageEvent{Event: api.FetchImageEventEventDone, Image: &api.ImageManifestEntry{
				Name: name, Sha256: &e.SHA256, Size: &e.Size, Path: ptrTo(rc.final)}})
			return
		}
	}

	resp, ff := h.openBlob(r.Context(), host, name, req)
	if ff != nil {
		fail(ff.reason, ff.msg)
		return
	}
	defer resp.Body.Close()

	total := req.Size
	step := max(1, min(total/100, progressMaxStep))
	src := &progressReader{r: resp.Body, step: step, next: step, emit: func(n int64) {
		st.send(api.FetchImageEvent{Event: api.FetchImageEventEventProgress, Received: ptrTo(n), Total: ptrTo(total)})
	}}
	entry, uerr := rc.receive(src, req.Size, req.Sha256)
	if uerr != nil {
		reason := api.Other
		if uerr.mismatch {
			reason = api.Mismatch
		}
		fail(reason, uerr.msg)
		return
	}
	slog.Info("fetchImage", "image", name, "outcome", "done", "bytes", total)
	st.send(api.FetchImageEvent{Event: api.FetchImageEventEventDone, Image: &entry})
}

func ptrTo[T any](v T) *T { return &v }

// --- the dial ---

// fetchFailure is a download that did not start or could not be used.
type fetchFailure struct {
	reason api.FetchImageEventReason
	msg    string
}

// openBlob asks the store for the image and returns its response when it is
// a 200 for the image the request describes: the store's Content-Length and
// X-Image-SHA256 are checked against the request before any body is read. The
// returned Body must be closed. The address is h.fetchURL and the fixed path
// only; the ticket is the Authorization header of this one request.
func (h *handlers) openBlob(ctx context.Context, host, name string, req api.FetchImageRequest) (*http.Response, *fetchFailure) {
	other := func(format string, a ...any) *fetchFailure {
		return &fetchFailure{reason: api.Other, msg: scrubTicket(fmt.Sprintf(format, a...), req.Ticket)}
	}
	base, err := url.Parse(h.fetchURL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" {
		return nil, other("store_url in config.yml is not an https URL with a host")
	}
	u := base.JoinPath("v1", "images", name, "blob")

	ctx, cancel := context.WithCancel(ctx)
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		cancel()
		return nil, other("build the store request: %v", err)
	}
	hr.Header.Set("Authorization", "Ticket "+req.Ticket)
	hr.Header.Set("Accept", "application/octet-stream")
	hr.Header.Set("User-Agent", "vmtool-daemon")

	tr := &http.Transport{
		Proxy:                 nil, // direct: no proxy may see the store address or the ticket
		DialContext:           (&net.Dialer{Timeout: fetchDialTimeout}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: h.fetchRoots},
		TLSHandshakeTimeout:   fetchDialTimeout,
		ResponseHeaderTimeout: fetchHeaderTimeout,
		DisableKeepAlives:     true,
		DisableCompression:    true, // Content-Length must be the image's
	}
	hc := &http.Client{
		Transport: tr,
		// Never follow a redirect: the ticket is for this one address.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := hc.Do(hr)
	if err != nil {
		cancel()
		tr.CloseIdleConnections()
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return nil, other("the request ended; fetch cancelled")
		}
		// *url.Error names the method, the URL and the cause, never a header.
		return nil, &fetchFailure{reason: api.StoreUnreachable,
			msg: scrubTicket(fmt.Sprintf("%s%s: %v", remote.StoreUnreachablePrefix, host, err), req.Ticket)}
	}
	orig := resp.Body
	closeAll := func() {
		orig.Close()
		cancel()
		tr.CloseIdleConnections()
	}
	refuse := func(format string, a ...any) (*http.Response, *fetchFailure) {
		closeAll()
		return nil, &fetchFailure{reason: api.StoreRefused, msg: scrubTicket(fmt.Sprintf(format, a...), req.Ticket)}
	}
	mismatch := func(format string, a ...any) (*http.Response, *fetchFailure) {
		closeAll()
		return nil, &fetchFailure{reason: api.Mismatch, msg: scrubTicket(fmt.Sprintf(format, a...), req.Ticket)}
	}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return refuse("the store answered a redirect (HTTP %d); vmtool does not follow redirects", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return refuse("the store answered HTTP %d %s (an expired or already used ticket is 401)", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if resp.ContentLength < 0 {
		return mismatch("the store did not state the image's size; %d bytes were expected", req.Size)
	}
	if resp.ContentLength != req.Size {
		return mismatch("the store's image is %d bytes, %d were expected (the image may have been replaced; ask again)", resp.ContentLength, req.Size)
	}
	if got := strings.ToLower(resp.Header.Get("X-Image-SHA256")); got != "" && got != req.Sha256 {
		if sha256Hex.MatchString(got) {
			return mismatch("the store's image has SHA-256 %s, %s was expected (the image may have been replaced; ask again)", got, req.Sha256)
		}
		return mismatch("the store's X-Image-SHA256 is not a SHA-256 digest; %s was expected", req.Sha256)
	}
	resp.Body = newIdleBody(resp.Body, fetchIdle, closeAll, cancel)
	return resp, nil
}

// idleBody fails a read that has waited longer than the idle time: a timer
// that is pushed forward on every read cancels the request's context.
type idleBody struct {
	rc    io.ReadCloser
	timer *time.Timer
	idle  time.Duration
	once  sync.Once
	close func()
}

func newIdleBody(rc io.ReadCloser, idle time.Duration, closeAll func(), cancel context.CancelFunc) *idleBody {
	return &idleBody{rc: rc, idle: idle, timer: time.AfterFunc(idle, cancel), close: closeAll}
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.timer.Reset(b.idle)
	return b.rc.Read(p)
}

func (b *idleBody) Close() error {
	b.once.Do(func() {
		b.timer.Stop()
		b.close()
	})
	return nil
}

// progressReader calls emit with the running total each time another step
// bytes have passed.
type progressReader struct {
	r          io.Reader
	n          int64
	step, next int64
	emit       func(n int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	for p.n >= p.next {
		p.emit(p.n)
		p.next += p.step
	}
	return n, err
}

// --- NDJSON ---

// ndjsonStream writes one JSON object per line and flushes after each. The
// 200 and the headers go out with the first line.
type ndjsonStream struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
}

func newNDJSON(w http.ResponseWriter) *ndjsonStream {
	return &ndjsonStream{w: w, rc: http.NewResponseController(w)}
}

func (s *ndjsonStream) send(ev any) {
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if !s.started {
		s.started = true
		s.w.Header().Set("Content-Type", "application/x-ndjson")
		s.w.Header().Set("Cache-Control", "no-store")
		s.w.WriteHeader(http.StatusOK)
	}
	_, _ = s.w.Write(append(b, '\n'))
	_ = s.rc.Flush()
}

// --- a workstation forwards ---

// forwardFetch serves fetchImage on a workstation for a listed host: the
// request goes to that host's daemon (remote.Manager.FetchImage) and its
// NDJSON lines are relayed as they arrive.
func (h *handlers) forwardFetch(w http.ResponseWriter, r *http.Request, host, name string) {
	if !vmtool.ValidImageName(name) {
		writeFetchStatus(w, http.StatusBadRequest, badImageName(name))
		return
	}
	req, bad := decodeFetchRequest(r)
	if bad != "" {
		writeFetchStatus(w, http.StatusBadRequest, bad)
		return
	}
	m, done, err := h.hosts.WithToken(requestToken(r.Context())).Resolve(host)
	if err != nil {
		code := http.StatusInternalServerError
		if kindOf(err) == app.KindNotFound {
			code = http.StatusNotFound
		}
		writeFetchStatus(w, code, scrubTicket(err.Error(), req.Ticket))
		return
	}
	defer done()
	f, ok := m.(imageFetcher)
	if !ok {
		writeFetchStatus(w, http.StatusInternalServerError, fmt.Sprintf("host %s keeps no image cache", host))
		return
	}
	st := newNDJSON(w)
	terminal := false
	err = f.FetchImage(r.Context(), name, req, func(ev api.FetchImageEvent) {
		if ev.Event != api.FetchImageEventEventProgress {
			terminal = true
		}
		st.send(ev)
	})
	if err == nil || terminal {
		return
	}
	msg := scrubTicket(err.Error(), req.Ticket)
	if st0 := hostFailure(err); st0 != 0 && !st.started {
		// the host did not answer and no line was sent: a status, not a line
		slog.Warn("fetchImage forward", "image", name, "host", host, "err", msg)
		writeFetchStatus(w, st0, msg)
		return
	}
	var fe *remote.FetchError
	if st.started || !errors.As(err, &fe) && !isStatusErr(err) {
		// The stream began (or the call never got an answer): the failure is a line.
		reason := api.Other
		if errors.As(err, &fe) && fe.Kind == remote.FetchStoreUnreachable {
			reason = api.StoreUnreachable
		}
		slog.Warn("fetchImage forward", "image", name, "host", host, "err", msg)
		st.send(api.FetchImageEvent{Event: api.FetchImageEventEventError, Error: &msg, Reason: &reason})
		return
	}
	switch {
	case fe != nil && fe.Kind == remote.FetchNoStore:
		writeFetchStatus(w, http.StatusConflict, fe.Msg)
	case fe != nil && fe.Kind == remote.FetchDaemonTooOld:
		writeFetchStatus(w, http.StatusNotFound, fmt.Sprintf("fetchImage is not supported by the daemon on %s (it predates API 2.3.0)", host))
	default:
		code := http.StatusInternalServerError
		switch kindOf(err) {
		case app.KindInvalid:
			code = http.StatusBadRequest
		case app.KindNotFound:
			code = http.StatusNotFound
		case app.KindConflict:
			code = http.StatusConflict
		}
		writeFetchStatus(w, code, msg)
	}
}

// isStatusErr reports whether err is a daemon's HTTP refusal.
func isStatusErr(err error) bool {
	var se *remote.StatusError
	return errors.As(err, &se)
}
