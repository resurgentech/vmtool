package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/resurgentech/keyclient/keylist"
	"github.com/resurgentech/vmtool/internal/api"
)

// keyRole is the one role of the pulled key list this daemon accepts.
const keyRole = "full"

// maxLastErrorRunes caps last_error of GET /v1/personal-keys.
const maxLastErrorRunes = 200

// localTokenLogEvery is the least time between two "local token used" lines.
const localTokenLogEvery = time.Hour

// clock is the server's time source (tests inject one).
func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// initKeys builds, once, what authentication needs besides token_sha256: the
// local token's use counter and, when config.yml sets personal_keys.url, the
// key list's Source. It does not start the Source (startKeys) and it never
// touches the network or a file. The error is a configuration error and
// holds no token.
func (s *Server) initKeys() error {
	if s.keysInit {
		return s.keysErr
	}
	s.keysInit = true
	s.local = &localTokenUse{now: s.clock}
	if s.Config == nil || s.Config.PersonalKeys.URL == "" {
		return nil
	}
	c, p := s.Config, s.Config.PersonalKeys
	local := strings.ToLower(c.TokenSHA256)
	src, err := keylist.New(keylist.Options{
		URL:               p.URL,
		TokenFile:         c.Path(p.TokenFile),
		CacheFile:         c.Path(p.CacheFile),
		Refresh:           p.Refresh,
		RefreshOnMiss:     p.RefreshOnMiss,
		AllowInsecureHTTP: p.AllowInsecureHTTP,
		// An entry with token_sha256's hash is skipped: the local token
		// wins, and is counted as the local token.
		Accept: keylist.AcceptRoles([]string{keyRole}, func(e keylist.Entry) bool {
			return local != "" && strings.ToLower(e.SHA256) == local
		}),
		Logger: slog.Default(),
	})
	if err != nil {
		s.keysErr = fmt.Errorf("personal_keys: %w", err)
		return s.keysErr
	}
	s.keys = src
	return nil
}

// startKeys starts the key list, if there is one: it loads the cache, logs
// the one start-up line and pulls in the background until ctx ends. It never
// fails because the provider is down.
func (s *Server) startKeys(ctx context.Context) error {
	if err := s.initKeys(); err != nil {
		return err
	}
	if s.keys == nil {
		return nil
	}
	if err := s.keys.Start(ctx); err != nil {
		return fmt.Errorf("personal_keys: %w", err)
	}
	st := s.keys.Status()
	slog.Info(fmt.Sprintf("personal_keys: list origin=%s entries=%d age=%s", st.Origin, st.Entries, st.Age.Round(time.Second)))
	return nil
}

// authenticator decides whether a bearer token is accepted: token_sha256
// first, then the pulled key list when there is one.
type authenticator struct {
	hash  string
	keys  *keylist.Source
	local *localTokenUse
}

// localTokenUse counts the requests token_sha256 authenticated since the
// process started, remembers the time of the last one and logs one line at
// most once an hour. It is safe for concurrent use and cheap: one short
// critical section per authenticated request.
type localTokenUse struct {
	now func() time.Time

	mu     sync.Mutex
	count  int64
	last   time.Time
	logged time.Time
}

// note records one request authenticated by the local token. The line names
// the route and nothing about the token.
func (u *localTokenUse) note(method, path string) {
	if u == nil {
		return
	}
	// Round(0) drops the monotonic reading: the hour is wall-clock time, so
	// a host that slept for hours logs at its next use.
	t := u.now().Round(0)
	u.mu.Lock()
	u.count++
	u.last = t
	d := t.Sub(u.logged)
	line := u.logged.IsZero() || d >= localTokenLogEvery || d < 0
	if line {
		u.logged = t
	}
	u.mu.Unlock()
	if line {
		slog.Info("local token used", "method", method, "path", path)
	}
}

// snapshot is the count and the time of the last use (zero if none).
func (u *localTokenUse) snapshot() (int64, time.Time) {
	if u == nil {
		return 0, time.Time{}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.count, u.last
}

// GetPersonalKeys answers GET /v1/personal-keys: 200 to any authenticated
// caller, with or without a key list.
func (h *handlers) GetPersonalKeys(_ context.Context, _ api.GetPersonalKeysRequestObject) (api.GetPersonalKeysResponseObject, error) {
	var out api.PersonalKeysStatus
	if h.keys != nil {
		out = keyStatus(h.keys.Status())
		out.Enabled = true
	}
	n, last := h.local.snapshot()
	out.LocalToken.Count = int(n)
	if !last.IsZero() {
		l := last.UTC()
		out.LocalToken.LastUsed = &l
	}
	return api.GetPersonalKeys200JSONResponse(out), nil
}

// keyStatus maps keyclient's Status to the status object: age and list_time
// only when a list is live, last_success only after a good pull, last_error
// only after a failed attempt, every non-printable rune replaced by '?' and
// cut to 200 runes. The local_token part is the caller's.
func keyStatus(st keylist.Status) api.PersonalKeysStatus {
	origin := string(st.Origin)
	entries, failing := st.Entries, st.CacheFailing
	out := api.PersonalKeysStatus{Origin: (*api.PersonalKeysStatusOrigin)(&origin), Entries: &entries, CacheFailing: &failing}
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
