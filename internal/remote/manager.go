package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const (
	// DefaultTimeout bounds one ordinary call to a daemon.
	DefaultTimeout = 30 * time.Second
	// LongTimeout bounds the slow libvirt operations: clone, resize,
	// migrate and wait-ip (whose own wait may be longer; see waitTimeout).
	LongTimeout = 30 * time.Minute
	// waitMargin is added to WaitForIP's own wait for the round trip.
	waitMargin = 30 * time.Second
	// maxWaitSeconds is the daemon's limit on timeout_seconds (waitVMIP).
	maxWaitSeconds = 3600
	// maxBody bounds a daemon response read into memory.
	maxBody = 16 << 20
)

// Option configures a Manager.
type Option func(*options)

type options struct {
	roots         *x509.CertPool
	timeout, long time.Duration
}

// WithRootCAs makes the client trust pool instead of the system trust store.
// Tests use it; production clients verify against the system store, where
// the private CA is installed.
func WithRootCAs(pool *x509.CertPool) Option { return func(o *options) { o.roots = pool } }

// WithTimeouts overrides DefaultTimeout and LongTimeout (tests use short
// ones). A zero value keeps the default.
func WithTimeouts(def, long time.Duration) Option {
	return func(o *options) {
		if def > 0 {
			o.timeout = def
		}
		if long > 0 {
			o.long = long
		}
	}
}

// Manager is app.Manager over HTTPS for one remote vmtool daemon: each method
// is one call to the operation spec/README.md's method map names for it. It
// carries the caller's bearer token (never logged, never in an error), trusts
// the system store unless WithRootCAs says otherwise, never follows a
// redirect and uses no proxy.
//
// It never reads GET /version before an operation and compares no API
// versions; /version is only a reachability probe (Probe).
type Manager struct {
	name, base, token string
	client            *api.ClientWithResponses
	transport         *http.Transport
	timeout, long     time.Duration
}

// New returns the client for the daemon named name (its vmtool host name) at
// baseURL (https://address:port), sending token as its bearer. It makes no
// network call.
func New(name, baseURL, token string, opts ...Option) (*Manager, error) {
	o := options{timeout: DefaultTimeout, long: LongTimeout}
	for _, f := range opts {
		f(&o)
	}
	tr := &http.Transport{
		Proxy:               nil, // direct: the daemon is on the lab network
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.roots},
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConnsPerHost: 4,
	}
	hc := &http.Client{
		Transport: tr,
		// Never follow a redirect: the token is for this daemon only.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	m := &Manager{
		name: name, base: strings.TrimSuffix(baseURL, "/"), token: token,
		transport: tr, timeout: o.timeout, long: o.long,
	}
	c, err := api.NewClientWithResponses(m.base,
		api.WithHTTPClient(hc),
		api.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
			// GET /version is public: the probe sends no credential.
			if r.URL.Path != "/version" {
				r.Header.Set("Authorization", "Bearer "+m.token)
			}
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("%s: client for %s: %w", name, m.base, err)
	}
	m.client = c
	return m, nil
}

// Name is the vmtool host this client talks to.
func (m *Manager) Name() string { return m.name }

// BaseURL is the daemon's https://address:port.
func (m *Manager) BaseURL() string { return m.base }

// Close drops the client's idle connections. The Manager stays usable.
func (m *Manager) Close() { m.transport.CloseIdleConnections() }

// ourVersion is info.version of the spec embedded in this binary.
var ourVersion = sync.OnceValues(func() (string, error) {
	sw, err := api.GetSwagger()
	if err != nil {
		return "", err
	}
	return sw.Info.Version, nil
})

// StatusError is a daemon's refusal: its HTTP status and its JSON error text.
// A 404 matches vmtool.ErrNotFound, so the app layer reports it as not found.
type StatusError struct {
	Host   string
	Status int
	Msg    string
}

func (e *StatusError) Error() string {
	switch {
	case e.Status == http.StatusUnauthorized:
		return fmt.Sprintf("token rejected by %s; run `just token-fetch` or `just deploy %s`", e.Host, e.Host)
	case e.Msg != "":
		return e.Host + ": " + e.Msg
	case e.Status == http.StatusForbidden:
		return fmt.Sprintf("%s: this operation is served by the workstation, not by %s", e.Host, e.Host)
	}
	return fmt.Sprintf("%s: daemon answered HTTP %d %s", e.Host, e.Status, http.StatusText(e.Status))
}

func (e *StatusError) Unwrap() error {
	if e.Status == http.StatusNotFound {
		return vmtool.ErrNotFound
	}
	return nil
}

// statusKind is the app layer's kind for a daemon status. 401, 403 and 5xx
// are the far end's problem as seen from here: bad gateway (500 stays
// internal: the daemon's own failure, reported as such).
func statusKind(status int) app.Kind {
	switch status {
	case http.StatusBadRequest:
		return app.KindInvalid
	case http.StatusNotFound:
		return app.KindNotFound
	case http.StatusConflict:
		return app.KindConflict
	case http.StatusInternalServerError:
		return app.KindInternal
	}
	return app.KindBadGateway
}

// fail wraps err as an *app.Error with no Op, so the workflow that called the
// Manager names the operation.
func fail(kind app.Kind, err error) error { return &app.Error{Kind: kind, Err: err} }

func (m *Manager) statusErr(status int, body []byte) error {
	var e api.Error
	msg := ""
	if json.Unmarshal(body, &e) == nil {
		msg = sanitize(e.Error)
	}
	return fail(statusKind(status), &StatusError{Host: m.name, Status: status, Msg: msg})
}

// transportErr is a call that got no answer. *url.Error names the URL and
// the cause, never the request headers, so the token cannot leak here.
func (m *Manager) transportErr(op string, timeout time.Duration, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fail(app.KindBadGateway, &timeoutError{msg: fmt.Sprintf("%s: %s timed out after %s", m.name, op, timeout)})
	}
	return fail(app.KindBadGateway, fmt.Errorf("%s: %s: %w", m.name, op, err))
}

// timeoutError is a call that got no answer within its timeout; the one
// retried call (ImageManifest) tells it from other failures with it.
type timeoutError struct{ msg string }

func (e *timeoutError) Error() string { return e.msg }

func isTimeout(err error) bool {
	var te *timeoutError
	return errors.As(err, &te)
}

// sanitize strips control characters and bounds a daemon's message.
func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 512 {
		s = s[:512] + "..."
	}
	return s
}

// call runs one operation with timeout.
func (m *Manager) call(op string, timeout time.Duration, out any, do func(ctx context.Context) (*http.Response, error)) error {
	return m.send(op, timeout, out, do)
}

// send runs do under timeout. A 2xx decodes into out (when non-nil); any
// other status is a *StatusError inside an *app.Error.
func (m *Manager) send(op string, timeout time.Duration, out any, do func(ctx context.Context) (*http.Response, error)) error {
	return m.sendCtx(context.Background(), op, timeout, out, do)
}

// sendCtx is send under parent as well as timeout.
func (m *Manager) sendCtx(parent context.Context, op string, timeout time.Duration, out any, do func(ctx context.Context) (*http.Response, error)) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	resp, err := do(ctx)
	if err != nil {
		return m.transportErr(op, timeout, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return m.transportErr(op, timeout, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return m.statusErr(resp.StatusCode, body)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fail(app.KindBadGateway, fmt.Errorf("%s: %s: decode response: %v", m.name, op, err))
	}
	return nil
}

// waitTimeout is WaitForIP's client timeout: the long timeout, or the
// daemon's own wait plus a margin when that is longer.
func (m *Manager) waitTimeout(wait time.Duration) time.Duration {
	return max(m.long, wait+waitMargin)
}

func fromAPI(v api.VMInfo) vmtool.VMInfo {
	out := vmtool.VMInfo{
		Name:      v.Name,
		State:     vmtool.VMState(v.State),
		VCPUs:     uint(max(v.Vcpus, 0)),
		Memory:    uint(max(v.Memory, 0)),
		IP:        v.Ip,
		Autostart: v.Autostart,
	}
	if v.Firmware != nil {
		out.Firmware = string(*v.Firmware)
	}
	if v.DiskCache != nil {
		out.DiskCache = string(*v.DiskCache)
	}
	if v.DiskBacking != nil {
		out.DiskBacking = string(*v.DiskBacking)
	}
	return out
}

// firmwareOrEFI maps an unset VMConfig.Firmware to the default, efi.
func firmwareOrEFI(s string) string {
	if s == "" {
		return vmtool.FirmwareEFI
	}
	return s
}

func ptr[T any](v T) *T { return &v }

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func positive(n uint) *int {
	if n == 0 {
		return nil
	}
	return ptr(int(n))
}

// --- app.Manager ---

// lifecycle runs a route that answers the VM's info; the client keeps only
// the error.
func (m *Manager) lifecycle(op string, do func(ctx context.Context) (*http.Response, error)) error {
	return m.call(op, m.timeout, nil, do)
}

func (m *Manager) Start(name string) error {
	return m.lifecycle("startVM", func(ctx context.Context) (*http.Response, error) {
		return m.client.StartVM(ctx, m.name, name)
	})
}

func (m *Manager) Stop(name string) error {
	return m.lifecycle("stopVM", func(ctx context.Context) (*http.Response, error) {
		return m.client.StopVM(ctx, m.name, name)
	})
}

func (m *Manager) Resume(name string) error {
	return m.lifecycle("resumeVM", func(ctx context.Context) (*http.Response, error) {
		return m.client.ResumeVM(ctx, m.name, name)
	})
}

func (m *Manager) Destroy(name string) error {
	return m.lifecycle("poweroffVM", func(ctx context.Context) (*http.Response, error) {
		return m.client.PoweroffVM(ctx, m.name, name)
	})
}

func (m *Manager) Reboot(name string) error {
	return m.lifecycle("rebootVM", func(ctx context.Context) (*http.Response, error) {
		return m.client.RebootVM(ctx, m.name, name)
	})
}

func (m *Manager) SetAutostart(name string, enabled bool) error {
	return m.lifecycle("setAutostart", func(ctx context.Context) (*http.Response, error) {
		return m.client.SetAutostart(ctx, m.name, name, api.AutostartRequest{Enabled: enabled})
	})
}

// Delete is the raw deleteVMDefinition: the workstation's DeleteVM workflow
// forgets the guest credentials itself.
func (m *Manager) Delete(name string, noclone bool) error {
	return m.call("deleteVMDefinition", m.timeout, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.DeleteVMDefinition(ctx, m.name, name, &api.DeleteVMDefinitionParams{Noclone: ptr(noclone)})
	})
}

func (m *Manager) Info(name string) (*vmtool.VMInfo, error) {
	var v api.VMInfo
	if err := m.call("getVM", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.GetVM(ctx, m.name, name)
	}); err != nil {
		return nil, err
	}
	info := fromAPI(v)
	return &info, nil
}

func (m *Manager) List() ([]vmtool.VMInfo, error) {
	var vs []api.VMInfo
	if err := m.call("listVMs", m.timeout, &vs, func(ctx context.Context) (*http.Response, error) {
		return m.client.ListVMs(ctx, m.name)
	}); err != nil {
		return nil, err
	}
	out := make([]vmtool.VMInfo, len(vs))
	for i, v := range vs {
		out[i] = fromAPI(v)
	}
	return out, nil
}

// WaitForIP asks the daemon to wait up to timeout (whole seconds, within the
// daemon's 1..3600 range) and waits a little longer itself.
func (m *Manager) WaitForIP(name string, timeout time.Duration) (string, error) {
	secs := int(math.Ceil(timeout.Seconds()))
	secs = min(max(secs, 1), maxWaitSeconds)
	var v api.WaitIPResult
	if err := m.call("waitVMIP", m.waitTimeout(time.Duration(secs)*time.Second), &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.WaitVMIP(ctx, m.name, name, api.WaitIPRequest{TimeoutSeconds: &secs})
	}); err != nil {
		return "", err
	}
	return v.Ip, nil
}

// ResizeDisk is the raw resizeVMVolume (libvirt only); the workstation's
// ResizeDisk workflow grows the guest itself.
func (m *Manager) ResizeDisk(name string, sizeGB uint) error {
	return m.call("resizeVMVolume", m.long, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.ResizeVMVolume(ctx, m.name, name, api.ResizeVMVolumeRequest{SizeGb: int(sizeGB)})
	})
}

func (m *Manager) MigrateDisk(name, pool string) error {
	return m.call("migrateDisk", m.long, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.MigrateDisk(ctx, m.name, name, api.MigrateDiskRequest{Pool: pool})
	})
}

func (m *Manager) CreateVolume(pool, name string, sizeGB uint) (string, error) {
	var v api.PathResult
	if err := m.call("createVolume", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.CreateVolume(ctx, m.name, api.CreateVolumeRequest{Name: name, Pool: nonEmpty(pool), SizeGb: int(sizeGB)})
	}); err != nil {
		return "", err
	}
	return v.Path, nil
}

func (m *Manager) AddDisk(name string, sizeGB uint, pool string) (*vmtool.DiskDevice, error) {
	var v api.AddDiskResult
	if err := m.call("addDisk", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.AddDisk(ctx, m.name, name, api.AddDiskRequest{Size: int(sizeGB), Pool: nonEmpty(pool)})
	}); err != nil {
		return nil, err
	}
	return &vmtool.DiskDevice{Path: v.Path, Target: v.Target}, nil
}

func (m *Manager) ImagePath(name string) (string, error) {
	var v api.PathResult
	if err := m.call("getImagePath", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.GetImagePath(ctx, m.name, name)
	}); err != nil {
		return "", err
	}
	return v.Path, nil
}

func (m *Manager) ListNetworks() ([]string, error) {
	var v []string
	err := m.call("listNetworks", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.ListNetworks(ctx, m.name)
	})
	return v, err
}

func (m *Manager) ListBridges() ([]string, error) {
	var v []string
	err := m.call("listBridges", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.ListBridges(ctx, m.name)
	})
	return v, err
}

func (m *Manager) ListImagesByPool() (map[string][]string, error) {
	var v map[string][]string
	err := m.call("listImages", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.ListImages(ctx, m.name)
	})
	return v, err
}

func (m *Manager) DeleteImage(name, pool string) error {
	return m.call("deleteImage", m.timeout, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.DeleteImage(ctx, m.name, name, &api.DeleteImageParams{Pool: nonEmpty(pool)})
	})
}

func (m *Manager) ListPools() ([]vmtool.PoolInfo, error) {
	var vs []api.PoolInfo
	if err := m.call("listPools", m.timeout, &vs, func(ctx context.Context) (*http.Response, error) {
		return m.client.ListPools(ctx, m.name)
	}); err != nil {
		return nil, err
	}
	out := make([]vmtool.PoolInfo, len(vs))
	for i, p := range vs {
		out[i] = vmtool.PoolInfo{Name: p.Name, Path: p.Path, Active: p.Active}
		if p.Type != nil {
			out[i].Type = *p.Type
		}
	}
	return out, nil
}

func (m *Manager) CreatePool(name, path string) error {
	return m.call("createPool", m.timeout, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.CreatePool(ctx, m.name, api.CreatePoolRequest{Name: name, Path: path})
	})
}

func (m *Manager) CloneImage(basePath, newName, targetPool string) (string, error) {
	var v api.PathResult
	if err := m.call("cloneVolume", m.long, &v, func(ctx context.Context) (*http.Response, error) {
		return m.client.CloneVolume(ctx, m.name, api.CloneVolumeRequest{BasePath: basePath, NewName: newName, TargetPool: nonEmpty(targetPool)})
	}); err != nil {
		return "", err
	}
	return v.Path, nil
}

func (m *Manager) ResizeVolume(volPath string, sizeBytes uint64) error {
	if sizeBytes > math.MaxInt64 {
		return fail(app.KindInvalid, fmt.Errorf("%s: size %d bytes is too large", m.name, sizeBytes))
	}
	return m.call("resizeVolume", m.long, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.ResizeVolume(ctx, m.name, api.ResizeVolumeRequest{Path: volPath, SizeBytes: int64(sizeBytes)})
	})
}

// Define sends the domain definition. The VMConfig's SSHUser and SSHPass are
// guest credentials: they stay on the workstation and are never sent.
func (m *Manager) Define(cfg vmtool.VMConfig) error {
	netType := api.NetworkConfigType(cfg.Network.Type)
	if netType == "" {
		netType = api.NetworkConfigTypeNat // the libvirt wrapper's default
	}
	body := api.DefineVMRequest{
		DiskPath:        cfg.DiskPath,
		DiskSizeGb:      positive(cfg.DiskSizeGB),
		ExtraDiskPool:   nonEmpty(cfg.ExtraDiskPool),
		ExtraDiskSizeGb: positive(cfg.ExtraDiskSizeGB),
		MemoryGib:       int(cfg.Memory),
		Network:         api.NetworkConfig{Type: netType, Source: nonEmpty(cfg.Network.Source)},
		Pool:            nonEmpty(cfg.Pool),
		Vcpus:           int(cfg.VCPUs),
		// Always sent: the daemon defines the domain with exactly this.
		Firmware: ptr(api.DefineVMRequestFirmware(firmwareOrEFI(cfg.Firmware))),
	}
	if cfg.Network.MacvtapMode != "" {
		body.Network.MacvtapMode = ptr(api.NetworkConfigMacvtapMode(cfg.Network.MacvtapMode))
	}
	if cfg.DiskCache != "" {
		body.DiskCache = ptr(api.DefineVMRequestDiskCache(cfg.DiskCache))
	}
	if cfg.Noclone {
		body.Noclone = ptr(true)
	}
	if len(cfg.ExtraDisks) > 0 {
		disks := make([]api.DiskDevice, len(cfg.ExtraDisks))
		for i, d := range cfg.ExtraDisks {
			disks[i] = api.DiskDevice{Path: d.Path, Target: d.Target}
		}
		body.ExtraDisks = &disks
	}
	return m.call("defineVM", m.timeout, nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.DefineVM(ctx, m.name, cfg.Name, body)
	})
}

// --- app.ImageStore ---

var _ app.ImageStore = (*Manager)(nil)

// minUploadRate is the slowest upload the client waits for: an upload's
// timeout is LongTimeout plus one second per MiB of image (a 10 GiB image
// may take 30 min + ~2.8 h).
const minUploadRate = 1 << 20

// ImageManifest is the daemon's image-cache entry for name (getImageManifest).
// It is a read, so one timeout is retried once (a busy hypervisor can stall
// one answer); no other call is retried, and an error status never is.
func (m *Manager) ImageManifest(name string) (app.RemoteImage, error) {
	var v api.ImageManifestEntry
	do := func() error {
		v = api.ImageManifestEntry{}
		return m.call("getImageManifest", m.timeout, &v, func(ctx context.Context) (*http.Response, error) {
			return m.client.GetImageManifest(ctx, m.name, name)
		})
	}
	err := do()
	if err != nil && isTimeout(err) {
		err = do()
	}
	if err != nil {
		return app.RemoteImage{}, err
	}
	out := app.RemoteImage{Name: name, Foreign: v.Foreign}
	if v.Sha256 != nil && v.Size != nil {
		out.SHA256, out.Size = strings.ToLower(*v.Sha256), *v.Size
	}
	if v.Path != nil {
		out.Path = sanitize(*v.Path)
	}
	return out, nil
}

// uploadTimeout bounds an upload of size bytes.
func (m *Manager) uploadTimeout(size int64) time.Duration {
	return m.long + time.Duration(size/minUploadRate)*time.Second
}

// UploadImage streams size bytes of r to the daemon (uploadImage) with
// Content-Length set, never holding the image in memory. progress gets the
// bytes handed to the connection so far.
func (m *Manager) UploadImage(ctx context.Context, name string, r io.Reader, size int64, sha256 string, progress func(sent int64)) error {
	if size <= 0 {
		return fail(app.KindInvalid, fmt.Errorf("%s: upload %s: size must be > 0", m.name, name))
	}
	body := &countingReader{r: io.LimitReader(r, size), progress: progress}
	return m.sendCtx(ctx, "uploadImage", m.uploadTimeout(size), nil, func(ctx context.Context) (*http.Response, error) {
		return m.client.UploadImageWithBody(ctx, m.name, name, &api.UploadImageParams{XImageSHA256: sha256},
			"application/octet-stream", body, func(_ context.Context, req *http.Request) error {
				req.ContentLength = size
				return nil
			})
	})
}

// countingReader reports the running total of bytes read.
type countingReader struct {
	r        io.Reader
	sent     int64
	progress func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.sent += int64(n)
		if c.progress != nil {
			c.progress(c.sent)
		}
	}
	return n, err
}
