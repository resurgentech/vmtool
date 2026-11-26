// Package keycfg is the personal_keys settings and their validation, shared by
// the daemon's config.yml and the store's config.yaml. It imports nothing that
// links libvirt, so the store may use it.
package keycfg

import (
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Defaults of the pulled key list's timings. The URL has no default.
const (
	DefaultRefresh       = time.Minute
	DefaultRefreshOnMiss = 5 * time.Second
)

// Settings is the personal_keys: block of config.yml (daemon) and config.yaml
// (store): the settings for per-person API keys. Every key is off unless set; none has a default value
// except the two timings of a list that is on.
type Settings struct {
	// RequestURL is the absolute URL where a tool files a key request. The
	// daemon only tells tools about it (GET /v1/key-request, public); it
	// never dials it. Absolute https, or plain http to a loopback host; no
	// user info, no fragment. Unset: that route answers 404.
	RequestURL string `yaml:"request_url"`

	// URL is the address of the pulled key list. Unset: no list, no pull, no
	// cache file, and every other key below must be unset too. Absolute http
	// or https with a host and no user info; plain http only to a loopback
	// host unless AllowInsecureHTTP is set.
	URL string `yaml:"url"`
	// TokenFile holds the pull token (relative to the config directory,
	// like tls_cert). Optional: the provider may need none.
	TokenFile string `yaml:"token_file"`
	// CacheFile is where the last good list is kept (relative to the config
	// directory). Required when URL is set.
	CacheFile string `yaml:"cache_file"`
	// Refresh is the time between pulls: default 1m, greater than zero.
	Refresh time.Duration `yaml:"refresh"`
	// RefreshOnMiss is the least time between two pulls that an unknown key
	// starts: default 5s; 0s turns refresh on miss off.
	RefreshOnMiss time.Duration `yaml:"refresh_on_miss"`
	// AllowInsecureHTTP allows a plain http URL to any host. The pull token
	// then travels in the clear, and the list decides who is let in: set it
	// only for a network path you trust.
	AllowInsecureHTTP bool `yaml:"allow_insecure_http"`
}

// Defaults is a Settings with the timings of a list that is on, so a file that
// names refresh_on_miss: 0s (off) is told from one that leaves it out. Decode
// into it.
func Defaults() Settings {
	return Settings{Refresh: DefaultRefresh, RefreshOnMiss: DefaultRefreshOnMiss}
}

// checkRequestURL validates personal_keys.request_url. The message never
// echoes the value: a URL can carry credentials.
func checkRequestURL(raw string) error {
	u, err := url.Parse(raw)
	ok := err == nil && u.Opaque == "" && u.Hostname() != "" && u.User == nil &&
		u.Fragment == "" && !strings.Contains(raw, "#") && (u.Scheme == "https" ||
		(u.Scheme == "http" && isLoopbackHost(u.Hostname())))
	if !ok {
		return errors.New("personal_keys.request_url must be an absolute https URL with a host, like https://keys.example.lab/v1/services/example/requests, or plain http to a loopback host (localhost, 127.0.0.0/8, ::1); no credentials, no fragment")
	}
	return nil
}

// Check validates the settings: request_url, then the pulled key list's. The
// messages name the setting and never echo a value: a URL or a path can carry
// a secret.
func (p Settings) Check() error {
	if p.RequestURL != "" {
		if err := checkRequestURL(p.RequestURL); err != nil {
			return err
		}
	}
	return p.checkList()
}

func (p Settings) checkList() error {
	if p.URL == "" {
		if p.TokenFile != "" || p.CacheFile != "" || p.AllowInsecureHTTP {
			return errors.New("personal_keys.token_file, cache_file and allow_insecure_http need personal_keys.url (the pulled key list is off without it)")
		}
		return nil
	}
	u, err := url.Parse(p.URL)
	if err != nil || u.Opaque != "" || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("personal_keys.url must be an absolute http(s) URL with a host, like https://keys.example.lab/keys/v1/lists/vmtool")
	}
	if u.User != nil {
		return errors.New("personal_keys.url must not carry credentials; put the pull token in personal_keys.token_file")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) && !p.AllowInsecureHTTP {
		return errors.New("personal_keys.url is plain http to a host that is not loopback; use https, or set personal_keys.allow_insecure_http: true only for a network path you trust")
	}
	if strings.TrimSpace(p.CacheFile) == "" {
		return errors.New("personal_keys.cache_file is required when personal_keys.url is set")
	}
	if p.Refresh <= 0 {
		return errors.New("personal_keys.refresh must be greater than zero (default 1m)")
	}
	if p.RefreshOnMiss < 0 {
		return errors.New("personal_keys.refresh_on_miss must not be negative (0s turns refresh on miss off; default 5s)")
	}
	return nil
}

// isLoopbackHost reports whether host is "localhost" or a loopback address.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}
