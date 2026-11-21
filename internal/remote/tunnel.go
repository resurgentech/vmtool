// Package remote is the client side of another host's vmtool daemon.
package remote

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TunnelProtocol is the Upgrade token of GET /v1/hosts/{host}/vms/{name}/tunnel.
const TunnelProtocol = "vmtool-tunnel"

// maxErrBody bounds how much of a refusal's body Dial reads.
const maxErrBody = 64 << 10

// Dial opens a tunnel to vm's TCP port 22 through the daemon at baseURL
// (https://address:port): GET /v1/hosts/<host>/vms/<vm>/tunnel over HTTP/1.1
// with the bearer token and `Connection: Upgrade`, `Upgrade: vmtool-tunnel`.
// On 101 the returned stream carries the guest's SSH bytes verbatim; it
// outlives ctx, which bounds only the connect and the response headers.
// Any other status is an error naming the status and the daemon's message.
//
// tlsConfig may carry RootCAs (tests); nil RootCAs is the system trust
// store. Dial always forces ALPN http/1.1: HTTP/2 cannot upgrade.
func Dial(ctx context.Context, baseURL, host, vm, token string, tlsConfig *tls.Config) (io.ReadWriteCloser, error) {
	var tc *tls.Config
	if tlsConfig != nil {
		tc = tlsConfig.Clone()
	} else {
		tc = &tls.Config{}
	}
	tc.NextProtos = []string{"http/1.1"}
	if tc.MinVersion < tls.VersionTLS12 {
		tc.MinVersion = tls.VersionTLS12
	}
	tr := &http.Transport{
		Proxy:             nil, // direct: the daemon is on the lab network
		DialContext:       (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSClientConfig:   tc,
		ForceAttemptHTTP2: false,
		// One request per transport: nothing to keep idle.
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	client := &http.Client{
		Transport: tr,
		// Never follow a redirect: the token is for this daemon only.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	u := strings.TrimSuffix(baseURL, "/") + "/v1/hosts/" + url.PathEscape(host) + "/vms/" + url.PathEscape(vm) + "/tunnel"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("tunnel %s/%s: %w", host, vm, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", TunnelProtocol)

	resp, err := client.Do(req)
	if err != nil {
		// *url.Error names the URL, never the headers.
		return nil, fmt.Errorf("tunnel %s/%s: %w", host, vm, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return nil, statusError(host, vm, token, resp)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), TunnelProtocol) {
		resp.Body.Close()
		return nil, fmt.Errorf("tunnel %s/%s: daemon switched to %q, want %s", host, vm, resp.Header.Get("Upgrade"), TunnelProtocol)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, fmt.Errorf("tunnel %s/%s: 101 response body is not writable", host, vm)
	}
	return rwc, nil
}

// statusError reads the daemon's JSON error (bounded) into an error, with the
// client's token removed from its text.
func statusError(host, vm, token string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	var e struct {
		Error string `json:"error"`
	}
	msg := ""
	if json.Unmarshal(body, &e) == nil {
		msg = e.Error
	}
	msg = cleanText(msg, token)
	if msg == "" {
		return fmt.Errorf("tunnel %s/%s: daemon answered %s", host, vm, resp.Status)
	}
	return fmt.Errorf("tunnel %s/%s: daemon answered %s: %s", host, vm, resp.Status, msg)
}

// Pipe copies stdin to conn and conn to stdout until the remote side closes
// conn (or ctx ends), then closes conn and returns. On stdin EOF it half-
// closes conn when conn supports CloseWrite (a TCP connection), so the far
// side can finish; otherwise it closes conn, which ends the tunnel. As an
// SSH ProxyCommand, ssh closing stdin therefore ends the tunnel.
//
// Pipe does not wait for the stdin copy: a blocked read of a terminal or
// pipe cannot be interrupted, and the process exits after Pipe returns.
func Pipe(ctx context.Context, conn io.ReadWriteCloser, stdin io.Reader, stdout io.Writer) error {
	var once sync.Once
	closeConn := func() { once.Do(func() { conn.Close() }) }
	defer closeConn()

	stop := context.AfterFunc(ctx, closeConn)
	defer stop()

	go func() {
		_, err := io.Copy(conn, stdin)
		if err == nil {
			if cw, ok := conn.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
				return
			}
		}
		closeConn()
	}()

	_, err := io.Copy(stdout, conn)
	if err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || ctx.Err() != nil {
		return nil
	}
	return err
}
