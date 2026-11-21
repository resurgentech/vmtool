// Package storeclient is the HTTP client for vmtool-store (spec/openapi/store.yaml),
// over the generated internal/storeapi client. It is an HTTP client only: no
// workflow, no files, no libvirt.
//
// The token travels only in the Authorization: Bearer header of authenticated
// routes (GET /version is public and gets none). It is in no URL, query or
// body, and no error returned here carries it or any part of a response body:
// errors say the operation and the HTTP status. TLS verifies against the
// system trust store unless WithRootCAs says otherwise (tests); redirects are
// never followed and no proxy is used.
package storeclient

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
	"strings"
	"time"

	"github.com/jaredeh/vmtool/internal/storeapi"
)

var (
	// ErrUnauthorized is the store refusing the token (401).
	ErrUnauthorized = errors.New("store refused the token")
	// ErrNotFound is a 404: no such image, VM record or machine.
	ErrNotFound = errors.New("not found in the store")
	// ErrStale wraps ErrNotFound: a VM record exists, for another UUID (the VM
	// was recreated). Its credentials must not be used.
	ErrStale = fmt.Errorf("%w: the record is for another VM with this name (stale)", ErrNotFound)
	// ErrForbidden is a 403: the token is valid but may not do this (a host
	// token for another host, or a user token on the report route).
	ErrForbidden = errors.New("store refused this token for that operation")
	// ErrConflict is a 409: another push or delete of this image is running.
	ErrConflict = errors.New("store: conflicting operation in progress")
)

// Types of the store API, so callers need not import internal/storeapi.
type (
	Image             = storeapi.Image
	Ticket            = storeapi.Ticket
	VMCredential      = storeapi.VMCredential
	VMCredentialInput = storeapi.VMCredentialInput
	VMCredentialRef   = storeapi.VMCredentialRef
	MachineCredential = storeapi.MachineCredential
	MachineInput      = storeapi.MachineCredentialInput
	MachineInputKind  = storeapi.MachineCredentialInputKind
	MachineRef        = storeapi.MachineRef
	MachineSummary    = storeapi.MachineSummary
	HostReport        = storeapi.HostReport
	ReportVM          = storeapi.ReportVM
	ReportResult      = storeapi.ReportResult
	Inventory         = storeapi.Inventory
	InventoryVM       = storeapi.InventoryVM
	Delisted          = storeapi.Delisted
)

// Option configures a Client.
type Option func(*options)

type options struct{ roots *x509.CertPool }

// WithRootCAs makes the client trust pool instead of the system trust store.
// Tests use it; production clients verify against the system store.
func WithRootCAs(pool *x509.CertPool) Option { return func(o *options) { o.roots = pool } }

// Client is one store at one base URL with one token. It is safe for
// concurrent use.
type Client struct {
	base   string
	token  string
	api    *storeapi.ClientWithResponses
	closer func()
}

// New returns the client for the store at rawURL, sending token as its
// bearer. rawURL must be an https URL with a host. It makes no network call.
// Errors never echo rawURL or token.
func New(rawURL, token string, opts ...Option) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("store URL must be an https URL with a host")
	}
	var o options
	for _, f := range opts {
		f(&o)
	}
	tr := &http.Transport{
		Proxy:               nil, // direct: the store is on the lab network
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.roots},
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConnsPerHost: 4,
	}
	// No client timeout: an image moves for minutes. Callers bound a call
	// with its context.
	hc := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	c := &Client{base: strings.TrimSuffix(rawURL, "/"), token: token, closer: tr.CloseIdleConnections}
	// GET /version is public: the probe sends no credential. Only that exact
	// path; an image or machine named "version" still gets the bearer.
	versionPath := strings.TrimSuffix(u.Path, "/") + "/version"
	a, err := storeapi.NewClientWithResponses(c.base,
		storeapi.WithHTTPClient(hc),
		storeapi.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
			if r.URL.Path != versionPath {
				r.Header.Set("Authorization", "Bearer "+c.token)
			}
			return nil
		}))
	if err != nil {
		return nil, errors.New("store client: invalid URL")
	}
	c.api = a
	return c, nil
}

// Close drops idle connections.
func (c *Client) Close() { c.closer() }

// transportErr keeps the cause (a timeout, a refused connection, a TLS
// failure) and drops the request URL the net/http error wraps it in.
func transportErr(op string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("store %s: %w", op, err)
}

// statusErr is the error for an unexpected HTTP status: the operation and the
// code, never the body.
func statusErr(op string, code int) error {
	switch code {
	case http.StatusUnauthorized:
		return fmt.Errorf("store %s: %w", op, ErrUnauthorized)
	case http.StatusNotFound:
		return fmt.Errorf("store %s: %w", op, ErrNotFound)
	case http.StatusForbidden:
		return fmt.Errorf("store %s: %w", op, ErrForbidden)
	case http.StatusConflict:
		return fmt.Errorf("store %s: %w", op, ErrConflict)
	}
	return fmt.Errorf("store %s: HTTP %d", op, code)
}

// Whoami is the name of the calling token (GET /v1/whoami).
func (c *Client) Whoami(ctx context.Context) (string, error) {
	r, err := c.api.WhoamiWithResponse(ctx)
	if err != nil {
		return "", transportErr("whoami", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return "", statusErr("whoami", r.StatusCode())
	}
	return r.JSON200.Name, nil
}

// Version is the store's version (GET /version, public).
func (c *Client) Version(ctx context.Context) (string, error) {
	r, err := c.api.GetVersionWithResponse(ctx)
	if err != nil {
		return "", transportErr("version", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return "", statusErr("version", r.StatusCode())
	}
	return r.JSON200.Version, nil
}

// GetVMCredential is the record for (host, name) if it is for domain uuid. A
// record for another UUID answers ErrStale; none, ErrNotFound.
func (c *Client) GetVMCredential(ctx context.Context, host, name, uuid string) (*VMCredential, error) {
	r, err := c.api.GetVMCredentialWithResponse(ctx, host, name, &storeapi.GetVMCredentialParams{Uuid: uuid})
	if err != nil {
		return nil, transportErr("get VM credential", err)
	}
	if r.StatusCode() == http.StatusNotFound && r.JSON404 != nil && r.JSON404.Stale != nil && *r.JSON404.Stale {
		return nil, fmt.Errorf("store get VM credential: %w", ErrStale)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, statusErr("get VM credential", r.StatusCode())
	}
	return r.JSON200, nil
}

// PutVMCredential stores a VM's credentials, replacing any record for
// (host, name).
func (c *Client) PutVMCredential(ctx context.Context, host, name string, in VMCredentialInput) (*VMCredentialRef, error) {
	r, err := c.api.PutVMCredentialWithResponse(ctx, host, name, in)
	if err != nil {
		return nil, transportErr("put VM credential", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, statusErr("put VM credential", r.StatusCode())
	}
	return r.JSON200, nil
}

// DeleteVMCredential removes the record for (host, name), whatever its UUID.
func (c *Client) DeleteVMCredential(ctx context.Context, host, name string) error {
	r, err := c.api.DeleteVMCredentialWithResponse(ctx, host, name)
	if err != nil {
		return transportErr("delete VM credential", err)
	}
	if r.StatusCode() != http.StatusNoContent {
		return statusErr("delete VM credential", r.StatusCode())
	}
	return nil
}

// ListMachines lists metal and alien machine records (names and kinds).
func (c *Client) ListMachines(ctx context.Context) ([]MachineSummary, error) {
	r, err := c.api.ListMachineCredentialsWithResponse(ctx)
	if err != nil {
		return nil, transportErr("list machines", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, statusErr("list machines", r.StatusCode())
	}
	return r.JSON200.Machines, nil
}

// GetMachine is a machine's manifest and credentials.
func (c *Client) GetMachine(ctx context.Context, name string) (*MachineCredential, error) {
	r, err := c.api.GetMachineCredentialWithResponse(ctx, name)
	if err != nil {
		return nil, transportErr("get machine", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, statusErr("get machine", r.StatusCode())
	}
	return r.JSON200, nil
}

// PutMachine stores a machine's manifest and credentials.
func (c *Client) PutMachine(ctx context.Context, name string, in MachineInput) (*MachineRef, error) {
	r, err := c.api.PutMachineCredentialWithResponse(ctx, name, in)
	if err != nil {
		return nil, transportErr("put machine", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, statusErr("put machine", r.StatusCode())
	}
	return r.JSON200, nil
}

// DeleteMachine removes a machine's record.
func (c *Client) DeleteMachine(ctx context.Context, name string) error {
	r, err := c.api.DeleteMachineCredentialWithResponse(ctx, name)
	if err != nil {
		return transportErr("delete machine", err)
	}
	if r.StatusCode() != http.StatusNoContent {
		return statusErr("delete machine", r.StatusCode())
	}
	return nil
}

// ListImages lists the indexed images, ordered by name.
func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	r, err := c.api.ListImagesWithResponse(ctx)
	if err != nil {
		return nil, transportErr("list images", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, statusErr("list images", r.StatusCode())
	}
	return r.JSON200.Images, nil
}

// GetImage is one image's metadata.
func (c *Client) GetImage(ctx context.Context, name string) (*Image, error) {
	r, err := c.api.GetImageWithResponse(ctx, name)
	if err != nil {
		return nil, transportErr("get image", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, statusErr("get image", r.StatusCode())
	}
	return r.JSON200, nil
}

// DeleteImage removes an image.
func (c *Client) DeleteImage(ctx context.Context, name string) error {
	r, err := c.api.DeleteImageWithResponse(ctx, name)
	if err != nil {
		return transportErr("delete image", err)
	}
	if r.StatusCode() != http.StatusNoContent {
		return statusErr("delete image", r.StatusCode())
	}
	return nil
}

// PushImage streams body (exactly size bytes whose SHA-256 is sha256) to the
// store. The store indexes it only when both match.
func (c *Client) PushImage(ctx context.Context, name, sha256 string, size int64, body io.Reader) (*Image, error) {
	params := &storeapi.PutImageParams{XImageSHA256: sha256}
	// The generated request has no length for a plain reader (chunked, which
	// the store refuses with 411): set it.
	setLen := func(_ context.Context, r *http.Request) error {
		r.ContentLength = size
		if size == 0 {
			r.Body = http.NoBody
		}
		return nil
	}
	r, err := c.api.PutImageWithBodyWithResponse(ctx, name, params, "application/octet-stream", body, setLen)
	if err != nil {
		return nil, transportErr("push image", err)
	}
	switch {
	case r.JSON201 != nil && r.StatusCode() == http.StatusCreated:
		return r.JSON201, nil
	case r.JSON200 != nil && r.StatusCode() == http.StatusOK:
		return r.JSON200, nil
	}
	return nil, statusErr("push image", r.StatusCode())
}

// OpenImage starts downloading an image's bytes. The caller reads and closes
// the body; sha256 and size come from the response headers.
func (c *Client) OpenImage(ctx context.Context, name string) (body io.ReadCloser, sha256 string, size int64, err error) {
	res, err := c.api.ClientInterface.GetImageBlob(ctx, name)
	if err != nil {
		return nil, "", 0, transportErr("get image blob", err)
	}
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		res.Body.Close()
		return nil, "", 0, statusErr("get image blob", res.StatusCode)
	}
	return res.Body, res.Header.Get("X-Image-SHA256"), res.ContentLength, nil
}

// CreateImageTicket mints a one-use download ticket for an image. The ticket
// is a credential for the blob route only; never log it.
func (c *Client) CreateImageTicket(ctx context.Context, name string) (*Ticket, error) {
	r, err := c.api.CreateImageTicketWithResponse(ctx, name)
	if err != nil {
		return nil, transportErr("create image ticket", err)
	}
	if r.StatusCode() != http.StatusCreated || r.JSON201 == nil {
		return nil, statusErr("create image ticket", r.StatusCode())
	}
	return r.JSON201, nil
}

// PutHostInventory sends host's full VM list (PUT /v1/hosts/{host}/inventory).
// Only a host token for that host may; any other token is ErrForbidden.
func (c *Client) PutHostInventory(ctx context.Context, host string, in HostReport) (*ReportResult, error) {
	r, err := c.api.PutHostInventoryWithResponse(ctx, host, in)
	if err != nil {
		return nil, transportErr("put host inventory", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, statusErr("put host inventory", r.StatusCode())
	}
	return r.JSON200, nil
}

// Inventory reads the store's hosts, VMs and machines (GET /v1/inventory). An
// empty host means every host; gone adds the VMs their host no longer has.
func (c *Client) Inventory(ctx context.Context, host string, gone bool) (*Inventory, error) {
	p := &storeapi.GetInventoryParams{}
	if host != "" {
		p.Host = &host
	}
	if gone {
		p.Gone = &gone
	}
	r, err := c.api.GetInventoryWithResponse(ctx, p)
	if err != nil {
		return nil, transportErr("inventory", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, statusErr("inventory", r.StatusCode())
	}
	return r.JSON200, nil
}

// Delist marks the live row of (host, name) gone. ErrNotFound when there is
// no live row.
func (c *Client) Delist(ctx context.Context, host, name string) (*Delisted, error) {
	r, err := c.api.DelistVMWithResponse(ctx, host, name)
	if err != nil {
		return nil, transportErr("delist", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
		return nil, statusErr("delist", r.StatusCode())
	}
	return r.JSON200, nil
}

// DeleteInventoryRow removes the row (host, name, uuid) at once. ErrNotFound
// when there is none.
func (c *Client) DeleteInventoryRow(ctx context.Context, host, name, uuid string) error {
	r, err := c.api.DeleteInventoryRowWithResponse(ctx, host, name, &storeapi.DeleteInventoryRowParams{Uuid: uuid})
	if err != nil {
		return transportErr("delete inventory row", err)
	}
	if r.StatusCode() != http.StatusNoContent {
		return statusErr("delete inventory row", r.StatusCode())
	}
	return nil
}
