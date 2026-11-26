package config

import (
	"errors"
	"net/netip"
	"net/url"
	"strings"
)

// PersonalKeys is config.yml's personal_keys: block, the daemon's settings for
// per-person API keys. Every key is off unless set; none has a default value.
type PersonalKeys struct {
	// RequestURL is the absolute URL where a tool files a key request. The
	// daemon only tells tools about it (GET /v1/key-request, public); it
	// never dials it. Absolute https, or plain http to a loopback host; no
	// user info, no fragment. Unset: that route answers 404.
	RequestURL string `yaml:"request_url"`
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

// isLoopbackHost reports whether host is "localhost" or a loopback address.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}
