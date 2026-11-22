package app

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"

	"github.com/resurgentech/vmtool/internal/storeclient"
)

// Mode is where a command gets credentials, machine records and base images.
// It is decided once per command (per request for the loopback service) and
// does not change while the command runs.
type Mode int

const (
	// ModeLocal: this machine's .machines/, image_dir and pools. Also the mode
	// when no store is configured.
	ModeLocal Mode = iota
	// ModeStore: the store.
	ModeStore
)

func (m Mode) String() string {
	if m == ModeStore {
		return "store"
	}
	return "local"
}

// WhoamiTimeout bounds the authenticated GET /v1/whoami that decides the
// mode. A var so tests can shorten it.
var WhoamiTimeout = 3 * time.Second

const usingLocal = "using this machine's .machines/ and image_dir"

// StoreSession is one command's (or one request's) view of the store: it
// decides the mode on first use and remembers it. A nil *StoreSession is a
// command with no server: configured: ModeLocal, no notice, local sources.
// Safe for concurrent use.
type StoreSession struct {
	url, host string
	client    *storeclient.Client
	tokenErr  error // the store token could not be loaded: never probe
	notify    func(string)

	once sync.Once
	mu   sync.Mutex // guards the decision below
	mode Mode
	// decided is set once Mode has run (under mu).
	decided bool
	// caller is the name whoami answered; notice is the one line printed or
	// returned when the mode fell back to local.
	caller, notice string
	// noted is the lines Notify has delivered: each distinct line goes out once.
	noted map[string]bool
}

// OpenStoreSession is the session for a configured store at rawURL, or nil
// when rawURL is empty (no server: block). token loads the store token
// (VMTOOL_SERVER_TOKEN); a failure is not fatal: the command runs in local
// mode and says so. notify, if set, receives the one fallback line, once.
// opts configure the client (tests inject their CA). Nothing here touches the
// network until Mode, CredentialsFor, CatalogFor or Status runs.
func OpenStoreSession(rawURL string, token func() (string, error), opts []storeclient.Option, notify func(string)) *StoreSession {
	if rawURL == "" {
		return nil
	}
	s := &StoreSession{url: rawURL, host: hostOf(rawURL), notify: notify}
	tok, err := token()
	s.tokenErr = err
	if err != nil {
		tok = ""
	}
	c, cerr := storeclient.New(rawURL, tok, opts...)
	if cerr != nil && s.tokenErr == nil {
		s.tokenErr = cerr
	}
	s.client = c
	return s
}

// hostOf is the host of rawURL without port, for the notice. It never
// returns anything else of the URL.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "the store"
	}
	return u.Hostname()
}

// Mode decides on the first call (one authenticated GET /v1/whoami, at most
// WhoamiTimeout) and returns that answer on every later call, whatever the
// store does after. Store reachable: ModeStore. Otherwise ModeLocal and, once,
// the notice (passed to notify).
func (s *StoreSession) Mode(ctx context.Context) Mode {
	if s == nil {
		return ModeLocal
	}
	note := s.decide(ctx)
	if note != "" && s.notify != nil {
		s.notify(note)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// decide runs the mode probe once and returns the fallback line on the call
// that ran it, "" on every other.
func (s *StoreSession) decide(ctx context.Context) string {
	var note string
	s.once.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.mode = ModeLocal
		switch {
		case s.tokenErr != nil:
			note = fmt.Sprintf("%s: %v: %s", s.host, s.tokenErr, usingLocal)
		default:
			cctx, cancel := context.WithTimeout(ctx, WhoamiTimeout)
			defer cancel()
			name, err := s.client.Whoami(cctx)
			if err != nil {
				note = fmt.Sprintf("%s unreachable: %s", s.host, usingLocal)
			} else {
				s.mode, s.caller = ModeStore, name
			}
		}
		s.notice = note
		s.decided = true
	})
	return note
}

// requireStore decides the mode like Mode but never delivers the "using local"
// fallback line: for a command with no local mode, whose own error says what
// is wrong. It returns the store's client, or a reason (no credential in it)
// the store cannot be used.
func (s *StoreSession) requireStore(ctx context.Context) (*storeclient.Client, error) {
	if s == nil {
		return nil, errNoStoreConfigured
	}
	s.decide(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.mode == ModeStore:
		return s.client, nil
	case s.tokenErr != nil:
		return nil, fmt.Errorf("%s: %v", s.host, s.tokenErr)
	}
	return nil, fmt.Errorf("the store at %s is not reachable, or refused this token", s.host)
}

// Host is the store URL's host (no port, nothing else of the URL), for
// messages.
func (s *StoreSession) Host() string {
	if s == nil {
		return ""
	}
	return s.host
}

// Notify delivers one line to the command's user through the session's notify
// (stderr for the CLI, the warnings header for the loopback service), once: a
// line already delivered is dropped. It reports whether the line went out, so
// a caller with a second channel (create's progress stream) can follow it.
// The line must carry no credential. Safe for concurrent use.
func (s *StoreSession) Notify(line string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	if s.noted[line] {
		s.mu.Unlock()
		return false
	}
	if s.noted == nil {
		s.noted = map[string]bool{}
	}
	s.noted[line] = true
	s.mu.Unlock()
	if s.notify != nil {
		s.notify(line)
	}
	return true
}

// Caller is the name the store gave this command's token, or "" in local mode
// or before the mode is decided.
func (s *StoreSession) Caller() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caller
}

// Notice is the one fallback line, or "" when there was none (store mode, no
// server, or not decided yet).
func (s *StoreSession) Notice() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notice
}

// CredentialsFor decides the mode (if not yet decided) and returns that
// mode's source.
func (s *StoreSession) CredentialsFor(ctx context.Context) CredentialSource {
	if s.Mode(ctx) == ModeStore {
		return StoreCredentials{Client: s.client}
	}
	return LocalCredentials{}
}

// CatalogFor decides the mode (if not yet decided) and returns that mode's
// image catalog.
func (s *StoreSession) CatalogFor(ctx context.Context) ImageCatalog {
	if s.Mode(ctx) == ModeStore {
		return StoreImages{Client: s.client}
	}
	return LocalCatalog{}
}

// Credentials is a CredentialSource that decides the mode on its first call.
// Building it never touches the network, so a command that never needs a
// credential never pays for the probe.
func (s *StoreSession) Credentials() CredentialSource {
	if s == nil {
		return LocalCredentials{}
	}
	return &modeCredentials{s}
}

// Catalog is an ImageCatalog that decides the mode on its first call.
func (s *StoreSession) Catalog() ImageCatalog {
	if s == nil {
		return LocalCatalog{}
	}
	return &modeCatalog{s}
}

// StoreStatus is what `vmtool hosts` shows for the store.
type StoreStatus struct {
	// Host is the URL's host; URL is the configured URL (it has no
	// credentials: config validation refuses them).
	Host, URL string
	// Reachable: GET /version answered. Caller is the name whoami gave this
	// token ("" if the token was refused or missing); Version is the store's.
	Reachable       bool
	Caller, Version string
	// Err says why something is missing; it never carries a token.
	Err error
}

// Status probes the store for display: GET /version (public) and, with a
// token, GET /v1/whoami, each bounded by timeout. It does not decide the
// mode. s must not be nil (no server: no row).
func (s *StoreSession) Status(ctx context.Context, timeout time.Duration) StoreStatus {
	st := StoreStatus{Host: s.host, URL: s.url}
	if s.client == nil {
		st.Err = s.tokenErr
		return st
	}
	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := s.client.Version(vctx)
	if err != nil {
		st.Err = err
		return st
	}
	st.Reachable, st.Version = true, v
	if s.tokenErr != nil {
		st.Err = s.tokenErr
		return st
	}
	wctx, cancel2 := context.WithTimeout(ctx, timeout)
	defer cancel2()
	name, err := s.client.Whoami(wctx)
	if err != nil {
		st.Err = err
		return st
	}
	st.Caller = name
	return st
}

type modeCredentials struct{ s *StoreSession }

func (m *modeCredentials) GetVM(ctx context.Context, host, name, uuid string) (*VMCredential, error) {
	return m.s.CredentialsFor(ctx).GetVM(ctx, host, name, uuid)
}
func (m *modeCredentials) PutVM(ctx context.Context, host, name string, c VMCredential) error {
	return m.s.CredentialsFor(ctx).PutVM(ctx, host, name, c)
}
func (m *modeCredentials) DeleteVM(ctx context.Context, host, name string) error {
	return m.s.CredentialsFor(ctx).DeleteVM(ctx, host, name)
}
func (m *modeCredentials) ListMachines(ctx context.Context) ([]MachineSummary, error) {
	return m.s.CredentialsFor(ctx).ListMachines(ctx)
}
func (m *modeCredentials) GetMachine(ctx context.Context, name string) (*MachineRecord, error) {
	return m.s.CredentialsFor(ctx).GetMachine(ctx, name)
}
func (m *modeCredentials) PutMachine(ctx context.Context, r MachineRecord) error {
	return m.s.CredentialsFor(ctx).PutMachine(ctx, r)
}
func (m *modeCredentials) DeleteMachine(ctx context.Context, name string) error {
	return m.s.CredentialsFor(ctx).DeleteMachine(ctx, name)
}

type modeCatalog struct{ s *StoreSession }

func (m *modeCatalog) List(ctx context.Context) ([]StoreImage, error) {
	return m.s.CatalogFor(ctx).List(ctx)
}
func (m *modeCatalog) Get(ctx context.Context, name string) (*StoreImage, error) {
	return m.s.CatalogFor(ctx).Get(ctx, name)
}
func (m *modeCatalog) Delete(ctx context.Context, name string) error {
	return m.s.CatalogFor(ctx).Delete(ctx, name)
}
func (m *modeCatalog) Push(ctx context.Context, name, sha256 string, size int64, r io.Reader) (*StoreImage, error) {
	return m.s.CatalogFor(ctx).Push(ctx, name, sha256, size, r)
}
func (m *modeCatalog) Open(ctx context.Context, name string) (io.ReadCloser, StoreImage, error) {
	return m.s.CatalogFor(ctx).Open(ctx, name)
}
func (m *modeCatalog) Ticket(ctx context.Context, name string) (*ImageTicket, error) {
	return m.s.CatalogFor(ctx).Ticket(ctx, name)
}
