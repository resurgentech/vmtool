package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/resurgentech/keyclient/keylist"
	"github.com/resurgentech/vmtool/internal/storeapi"
)

// keyRole is the one role of the pulled key list this store accepts.
const keyRole = "full"

// maxLastErrorRunes caps last_error of GET /v1/personal-keys.
const maxLastErrorRunes = 200

// Start builds and starts the pulled key list, if personal_keys.url is set:
// it loads the cache, logs the one start-up line and pulls in the background
// until ctx ends. It never fails because the provider is down, only because
// the list cannot be built (a configuration error; the message holds no
// token). With the url unset it does nothing, not even log. It runs once; a
// later call returns the first call's result. Call it before the store listens.
func (s *Server) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return s.startErr
	}
	s.started = true
	s.startErr = s.startKeys(ctx)
	return s.startErr
}

func (s *Server) startKeys(ctx context.Context) error {
	p := s.cfg.PersonalKeys
	if p.URL == "" {
		return nil
	}
	src, err := keylist.New(keylist.Options{
		URL:               p.URL,
		TokenFile:         p.TokenFile,
		CacheFile:         p.CacheFile,
		Refresh:           p.Refresh,
		RefreshOnMiss:     p.RefreshOnMiss,
		AllowInsecureHTTP: p.AllowInsecureHTTP,
		Accept:            keylist.AcceptRoles([]string{keyRole}, s.takenLocally),
		Logger:            s.log,
	})
	if err != nil {
		return fmt.Errorf("personal_keys: %w", err)
	}
	if err := src.Start(ctx); err != nil {
		return fmt.Errorf("personal_keys: %w", err)
	}
	s.keys.Store(src)
	st := src.Status()
	s.log.Info(fmt.Sprintf("personal_keys: list origin=%s entries=%d age=%s", st.Origin, st.Entries, st.Age.Round(time.Second)))
	return nil
}

// takenLocally is the local-tokens-win rule: an entry whose name or hash is a
// tokens.yaml caller's or a host's is skipped. Hashes compare in lowercase.
// The config does not change while the store runs, so the answer is fixed.
func (s *Server) takenLocally(e keylist.Entry) bool {
	h := strings.ToLower(e.SHA256)
	for _, m := range []map[string]string{s.cfg.Tokens, s.cfg.HostTokens} {
		if _, ok := m[e.Name]; ok {
			return true
		}
		for _, v := range m {
			if strings.ToLower(v) == h {
				return true
			}
		}
	}
	return false
}

// bearerAuth is the order of bearer credentials: a tokens.yaml caller (every
// entry checked, in constant time); then a host token, whose answer is final
// (a host caller on a route in hostAllowed, otherwise no caller) so that a
// known host token never reaches the list and never starts a pull; and only a
// token that is neither goes to the pulled list, which may pull once on a
// miss. A pulled key's caller is its key name; names cannot contain '/', so
// they never look like a host's.
func (s *Server) bearerAuth(ctx context.Context, tok, pat string) (string, bool) {
	if name, ok := s.bearerCaller(tok); ok {
		s.local.note(name)
		return name, true
	}
	if s.hostMatch(tok) != "" {
		return s.hostCaller(tok, pat)
	}
	if src := s.keys.Load(); src != nil {
		if name, _, ok := src.LookupOrRefresh(ctx, tok); ok {
			return name, true
		}
	}
	return "", false
}

// localUse counts, per tokens.yaml caller, the requests its token
// authenticated since the process started, and when the last one was. The set
// of names is fixed at start, so the map is read without a lock and each
// counter has its own.
type localUse struct {
	now   func() time.Time
	names []string
	use   map[string]*callerUse
}

type callerUse struct {
	mu    sync.Mutex
	count int64
	last  time.Time
}

func newLocalUse(tokens map[string]string, now func() time.Time) *localUse {
	u := &localUse{now: now, use: make(map[string]*callerUse, len(tokens))}
	for n := range tokens {
		u.names = append(u.names, n)
		u.use[n] = &callerUse{}
	}
	sort.Strings(u.names)
	return u
}

func (u *localUse) note(name string) {
	c := u.use[name]
	if c == nil {
		return
	}
	t := u.now().UTC()
	c.mu.Lock()
	c.count++
	c.last = t
	c.mu.Unlock()
}

func (u *localUse) snapshot() []storeapi.LocalTokenUse {
	out := make([]storeapi.LocalTokenUse, 0, len(u.names))
	for _, n := range u.names {
		c := u.use[n]
		c.mu.Lock()
		row := storeapi.LocalTokenUse{Name: n, Count: int(c.count)}
		if !c.last.IsZero() {
			l := c.last
			row.LastUsed = &l
		}
		c.mu.Unlock()
		out = append(out, row)
	}
	return out
}

// GetPersonalKeys answers GET /v1/personal-keys: 200 to any user caller, with
// or without a key list. A host token never gets here (hostAllowed).
func (h *handlers) GetPersonalKeys(context.Context, storeapi.GetPersonalKeysRequestObject) (storeapi.GetPersonalKeysResponseObject, error) {
	var out storeapi.PersonalKeysStatus
	if src := h.srv.keys.Load(); src != nil {
		out = keyStatus(src.Status())
		out.Enabled = true
	}
	out.LocalTokens = h.srv.local.snapshot()
	return storeapi.GetPersonalKeys200JSONResponse(out), nil
}

// GetKeyRequestDiscovery answers the public GET /v1/key-request: where a tool
// files a request for an API key, from personal_keys.request_url. The store
// only reports it and never dials it. Unset: 404.
func (h *handlers) GetKeyRequestDiscovery(context.Context, storeapi.GetKeyRequestDiscoveryRequestObject) (storeapi.GetKeyRequestDiscoveryResponseObject, error) {
	if u := h.srv.cfg.PersonalKeys.RequestURL; u != "" {
		return storeapi.GetKeyRequestDiscovery200JSONResponse{RequestUrl: u}, nil
	}
	return storeapi.GetKeyRequestDiscovery404JSONResponse{NotFoundJSONResponse: storeapi.NotFoundJSONResponse{Error: "no key request URL is configured on this store"}}, nil
}

// keyStatus maps keyclient's Status to the status object: age and list_time
// only when a list is live, last_success only after a good pull, last_error
// only after a failed attempt, every non-printable rune replaced by '?' and
// cut to 200 runes. local_tokens is the caller's.
func keyStatus(st keylist.Status) storeapi.PersonalKeysStatus {
	origin := storeapi.PersonalKeysStatusOrigin(st.Origin)
	entries, failing := st.Entries, st.CacheFailing
	out := storeapi.PersonalKeysStatus{Origin: &origin, Entries: &entries, CacheFailing: &failing}
	if st.Origin != keylist.OriginNone {
		age := int(st.Age / time.Second)
		lt := st.ListTime.UTC()
		out.AgeSeconds, out.ListTime = &age, &lt
	}
	if !st.LastSuccess.IsZero() {
		ls := st.LastSuccess.UTC()
		out.LastSuccess = &ls
	}
	if st.LastError != nil {
		rs := make([]rune, 0, maxLastErrorRunes)
		for _, r := range st.LastError.Error() {
			if len(rs) == maxLastErrorRunes {
				break
			}
			if !unicode.IsPrint(r) {
				r = '?'
			}
			rs = append(rs, r)
		}
		msg := string(rs)
		out.LastError = &msg
	}
	return out
}
