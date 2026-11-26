package cli

// The client's key: where it comes from, the first-use request, and the check
// of a token-file key by the local service. docs/CLI.md and the README's key
// sections describe the behaviour; the order itself is in internal/config.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/resurgentech/keyclient/request"
	"github.com/resurgentech/vmtool/internal/buildinfo"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/spf13/cobra"
)

const (
	noKeyRequestFlag  = "no-key-request"
	keyRequestURLFlag = "key-request-url"
	// KeyRequestURLEnv is the environment variable form of --key-request-url.
	keyRequestURLEnv = "VMTOOL_KEY_REQUEST_URL"
	// keyRole is the role a command other than `key request` asks for.
	keyRole = "full"

	exitTempFail = 75

	keyPendingLine = "key request pending; run this again after approving"
	keyNewLine     = "run this again to file a new request"

	localCheckTimeout = 2 * time.Second
	probeTimeout      = 5 * time.Second
)

// Tests replace these.
var (
	// keyIsTerminal says whether this run may wait for a person: stderr is a
	// terminal.
	keyIsTerminal = func() bool { return request.IsTerminal(os.Stderr) }
	// keyRetryDelay is how long a fresh key's 401 waits before the one retry.
	keyRetryDelay = 5 * time.Second
	// localKeyCheck is the one authenticated call to the local service that
	// tells whether it accepts key. It returns the HTTP status.
	localKeyCheck = realLocalKeyCheck
	// keyProbeRoots, when set, replaces the system trust store for the calls
	// to hosts that `key check` makes.
	keyProbeRoots *x509.CertPool
)

func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// noTokenError is today's refusal when no key is found anywhere.
func (e *env) noTokenError() *gateError {
	return &gateError{err: config.ErrNoToken, msg: fmt.Sprintf("vmtool: no %s in the environment or %s; run 'just token' (new) or 'just token-fetch' (from OpenBao)",
		config.TokenEnv, filepath.Join(e.dir, config.EnvFileName))}
}

// keyRequestParams are the request URLs and the token file of this run. The
// config is optional: a key request needs no config.yml when the URL is given.
func (e *env) keyRequestParams(cmd *cobra.Command) (serviceURL, override, tokenPath string, err error) {
	tokenPath, err = config.TokenFilePath(e.dir)
	if err != nil {
		return "", "", "", err
	}
	if cfg, lerr := config.Load(e.dir); lerr == nil && cfg.StoreConfigured() {
		serviceURL = cfg.Server.URL
	}
	override, _ = cmd.Flags().GetString(keyRequestURLFlag)
	if override == "" {
		override = os.Getenv(keyRequestURLEnv)
	}
	return serviceURL, override, tokenPath, nil
}

func nameHint() string {
	h, _ := config.ShortHostname()
	if u := os.Getenv("USER"); u != "" {
		return u + "@" + h
	}
	return h
}

// requestKey is request.Ensure with vmtool's options. The notice goes to
// stderr only when this run waits; otherwise the caller returns it in the
// error.
func (e *env) requestKey(cmd *cobra.Command, role string) (request.Result, string, error) {
	serviceURL, override, tokenPath, err := e.keyRequestParams(cmd)
	if err != nil {
		return request.Result{}, "", err
	}
	wait := keyIsTerminal()
	w := cmd.ErrOrStderr()
	res, err := request.Ensure(cmdContext(cmd), request.Options{
		ServiceURL: serviceURL, Override: override, TokenPath: tokenPath,
		Role: role, NameHint: nameHint(), Tool: "vmtool/" + buildinfo.Version(),
		Wait: wait,
		Notify: func(n request.Notice) {
			if wait {
				fmt.Fprintln(w, n.String())
			}
		},
	})
	return res, tokenPath, err
}

// classifyKeyErr maps an Ensure error to the lines, exit code and error code
// of the convention. ok is false for what the caller handles itself.
func classifyKeyErr(err error, tokenPath string) (lines []string, exit int, code string, ok bool) {
	var pending *request.ErrPending
	var perr *request.ProviderError
	switch {
	case errors.As(err, &pending):
		return []string{pending.Notice.String(), keyPendingLine}, exitTempFail, "key_pending", true
	case errors.Is(err, request.ErrBusy):
		return []string{"another process is handling the key request for " + tokenPath + "; try again shortly"}, exitTempFail, "key_busy", true
	case errors.Is(err, request.ErrDenied):
		return []string{"key request denied"}, exitError, "key_denied", true
	case errors.Is(err, request.ErrExpired):
		return []string{err.Error(), keyNewLine}, exitError, "key_expired", true
	case errors.Is(err, request.ErrConsumed):
		return []string{err.Error(), keyNewLine}, exitError, "key_consumed", true
	case errors.Is(err, request.ErrUnauthorized):
		return []string{err.Error()}, exitError, "key_unauthorized", true
	case errors.As(err, &perr):
		lines = []string{err.Error()}
		if perr.RetryAfter > 0 {
			lines = append(lines, fmt.Sprintf("try again in %ds", int((perr.RetryAfter+time.Second-1)/time.Second)))
		}
		return lines, exitError, "key_provider", true
	}
	return nil, 0, "", false
}

func keyErr(lines []string, exit int, code string) error {
	return &cliError{code: code, exit: exit, err: errors.New(strings.Join(lines, "\n"))}
}

// gate loads config.yml and the key and checks the key. A VMTOOL_TOKEN (the
// environment or .env) is checked against token_sha256, offline. A key from
// the token file is checked by the local service. With no key at all and a
// request URL, it files (or resumes) a key request. It fills e.cfg, e.token
// and e.tokenSource, or returns a *gateError (or a *cliError for the request
// outcomes).
func (e *env) gate(cmd *cobra.Command) error {
	key, src, err := config.DaemonKey(e.dir)
	fresh := false
	switch {
	case errors.Is(err, config.ErrNoToken):
		if off, _ := cmd.Flags().GetBool(noKeyRequestFlag); off {
			return e.noTokenError()
		}
		res, tokenPath, rerr := e.requestKey(cmd, keyRole)
		switch {
		case rerr == nil:
			key, src, fresh = res.Token, config.SourceFile, true
		case errors.Is(rerr, request.ErrNoProvider):
			return e.noTokenError()
		default:
			if lines, exit, code, ok := classifyKeyErr(rerr, tokenPath); ok {
				return keyErr(lines, exit, code)
			}
			ge := e.noTokenError()
			ge.msg += "; the key request failed: " + rerr.Error()
			return ge
		}
	case err != nil:
		return &gateError{err: err, msg: "vmtool: " + err.Error()}
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	// A VMTOOL_TOKEN that is not the operator's is not refused offline: it may
	// be a personal key (a daemon hands its ssh child the caller's key this
	// way), which only the local service can check. It is checked like a
	// token-file key; e.tokenSource keeps its real source.
	fromEnv := src != config.SourceFile && !config.Check(key, cfg.TokenSHA256)
	if src == config.SourceFile || fromEnv {
		if err := e.checkFileKey(cmdContext(cmd), cfg, key, fresh, fromEnv); err != nil {
			return err
		}
	}
	e.cfg, e.token, e.tokenSource = cfg, key, src
	return nil
}

// checkFileKey asks the local service whether it accepts key (the one place a
// key that is not the operator's is checked before a command runs): a
// token-file key, or, with fromEnv, a VMTOOL_TOKEN that is not token_sha256's.
// The call goes only to an address of this machine (errKeyCheckNotLocal).
// After a fresh key one 401 is retried once after keyRetryDelay: the service
// may not have pulled the list yet.
func (e *env) checkFileKey(ctx context.Context, cfg *config.Config, key string, fresh, fromEnv bool) error {
	status, err := localKeyCheck(ctx, cfg, key)
	if err == nil && status == http.StatusUnauthorized && fresh {
		select {
		case <-time.After(keyRetryDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
		status, err = localKeyCheck(ctx, cfg, key)
	}
	tokenPath, _ := config.TokenFilePath(e.dir)
	var notLocal *errKeyCheckNotLocal
	switch {
	case errors.As(err, &notLocal):
		return &gateError{err: err, msg: fmt.Sprintf("vmtool: the key was not sent: listen (%s) in %s is not an address of this machine, and the key is only checked by a service on this machine; fix listen, or use the operator token (token_sha256)",
			notLocal.addr, config.FileName)}
	case err != nil && fromEnv:
		return &gateError{err: config.ErrTokenMismatch, msg: fmt.Sprintf("vmtool: %s does not match token_sha256 in %s, and vmtool.service is not answering at %s to check it as a personal key; start it: sudo systemctl start vmtool",
			config.TokenEnv, config.FileName, localAddr(cfg))}
	case err != nil:
		return &gateError{err: err, msg: fmt.Sprintf("vmtool.service is not answering at %s; start it: sudo systemctl start vmtool", localAddr(cfg))}
	case status >= 200 && status < 300:
		return nil
	case (status == http.StatusUnauthorized || status == http.StatusForbidden) && fromEnv:
		return &gateError{err: config.ErrTokenMismatch, msg: fmt.Sprintf("vmtool: %s does not match token_sha256 in %s and the local service did not accept it either; run 'just token-fetch', or 'just install' after 'just token'",
			config.TokenEnv, config.FileName)}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &gateError{err: config.ErrTokenMismatch, msg: fmt.Sprintf("vmtool: the key in %s was not accepted by vmtool.service; run 'vmtool key status' to see where your key comes from, and 'vmtool key check' to see who accepts it", tokenPath)}
	}
	return &gateError{err: fmt.Errorf("HTTP %d", status), msg: fmt.Sprintf("vmtool.service at %s answered HTTP %d to the key check", localAddr(cfg), status)}
}

// errKeyCheckNotLocal is the dialer's refusal to connect the gate's key check
// to an address that is not this machine's. It holds the address, never the
// key.
type errKeyCheckNotLocal struct{ addr string }

func (e *errKeyCheckNotLocal) Error() string {
	return "address " + e.addr + " is not an address of this machine"
}

// machineIPs are the addresses assigned to this machine's interfaces. Tests
// replace it.
var machineIPs = func() []net.IP {
	addrs, _ := net.InterfaceAddrs()
	var out []net.IP
	for _, a := range addrs {
		switch v := a.(type) {
		case *net.IPNet:
			out = append(out, v.IP)
		case *net.IPAddr:
			out = append(out, v.IP)
		}
	}
	return out
}

// isThisMachine reports whether ip is loopback or assigned to an interface of
// this machine.
func isThisMachine(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	for _, m := range machineIPs() {
		if m.Equal(ip) {
			return true
		}
	}
	return false
}

// allowKeyCheckIP decides which addresses the gate's key check may connect to.
// Tests replace it to refuse even loopback.
var allowKeyCheckIP = isThisMachine

// localOnlyControl is a net.Dialer Control: it runs after name resolution and
// before the connect, so a refused address never gets a byte.
func localOnlyControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return &errKeyCheckNotLocal{addr: address}
	}
	if ip := net.ParseIP(host); ip == nil || !allowKeyCheckIP(ip) {
		return &errKeyCheckNotLocal{addr: address}
	}
	return nil
}

// localAddr is where the local workstation service listens, as a dialable
// address.
func localAddr(cfg *config.Config) string {
	addr := config.DefaultListen
	if cfg != nil && cfg.Listen != "" {
		addr = cfg.Listen
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// localClient reaches the local service. With tls_cert set it trusts exactly
// that certificate chain (and does not insist on a name: the address is
// loopback), as the service's own certificate is a private one.
func localClient(cfg *config.Config, timeout time.Duration) *http.Client {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg != nil && cfg.TLSCert != "" {
		pool := x509.NewCertPool()
		if pemBytes, err := os.ReadFile(cfg.Path(cfg.TLSCert)); err == nil {
			pool.AppendCertsFromPEM(pemBytes)
		}
		tc.InsecureSkipVerify = true // replaced by the chain check below
		tc.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			var certs []*x509.Certificate
			for _, r := range raw {
				c, err := x509.ParseCertificate(r)
				if err != nil {
					return err
				}
				certs = append(certs, c)
			}
			if len(certs) == 0 {
				return errors.New("no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range certs[1:] {
				inter.AddCert(c)
			}
			_, err := certs[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			return err
		}
	} else if keyProbeRoots != nil {
		tc.RootCAs = keyProbeRoots
	}
	return probeClientCtl(tc, timeout, localOnlyControl)
}

// probeClient is a client for any host (`key check`'s probes are remote):
// no "this machine" rule.
func probeClient(tc *tls.Config, timeout time.Duration) *http.Client {
	return probeClientCtl(tc, timeout, nil)
}

// probeClientCtl is probeClient with a dialer Control (nil: none).
func probeClientCtl(tc *tls.Config, timeout time.Duration, control func(network, address string, c syscall.RawConn) error) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, TLSClientConfig: tc, DisableKeepAlives: true,
			TLSHandshakeTimeout: timeout,
			DialContext:         (&net.Dialer{Timeout: timeout, Control: control}).DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// probe is one authenticated GET. It returns the status and the start of the
// body; an error never holds the URL or the key.
func probe(ctx context.Context, hc *http.Client, rawURL, key string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, errors.New("bad address")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, nil
}

func realLocalKeyCheck(ctx context.Context, cfg *config.Config, key string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, localCheckTimeout)
	defer cancel()
	st, _, err := probe(ctx, localClient(cfg, localCheckTimeout), "https://"+localAddr(cfg)+"/v1/personal-keys", key)
	return st, err
}
