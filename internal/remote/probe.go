package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/url"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
)

// OurVersion is info.version of the spec embedded in this binary: the API
// version this vmtool speaks.
func OurVersion() (string, error) { return ourVersion() }

// Probe asks the daemon for GET /version and reports what it learned: whether
// it answered, its api_version and the NotAfter of the leaf certificate it
// presented (read from the TLS connection state of that response). It sends
// no Authorization header and never returns an error: an unreachable host is
// a status with Err set. The api_version is informational; a different one
// sets no error. It is bounded by ctx and the client's default timeout.
func (m *Manager) Probe(ctx context.Context) app.HostStatus {
	st := app.HostStatus{Host: m.name}
	if u, err := url.Parse(m.base); err == nil {
		st.Address = u.Host
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	resp, err := m.client.GetVersion(ctx)
	if err != nil {
		st.Err = m.transportErr("getVersion", m.timeout, err)
		return st
	}
	defer resp.Body.Close()
	st.Reachable = true
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		st.CertNotAfter = resp.TLS.PeerCertificates[0].NotAfter
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		st.Reachable = false
		st.Err = m.transportErr("getVersion", m.timeout, err)
		return st
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		st.Err = m.statusErr(resp.StatusCode, body)
		return st
	}
	var v api.VersionInfo
	if err := json.Unmarshal(body, &v); err != nil {
		st.Err = fail(app.KindBadGateway, err)
		return st
	}
	st.APIVersion = sanitize(v.ApiVersion)
	return st
}

var _ app.Prober = (*Manager)(nil)
