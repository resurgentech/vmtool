// Package hosts resolves a host name to the app.Manager that drives it.
//
// The local host's Manager is the libvirt-backed vmtool.Manager, opened per
// Resolve and closed by the release func. On a hypervisor it is wrapped so an
// empty pool (the host's default) means the vmtool pool; see hypervisorPools. A host named in config.yml hosts:
// is a remote daemon: its Manager is a remote.Manager (HTTPS client) carrying
// the caller's token, which the resolver gets from WithToken (the CLI's
// verified token, or the bearer of the request a server is serving). No
// token is stored in the resolver a server shares between requests.
package hosts

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/remote"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// ErrUnknownHost is wrapped by Resolve for a host this server does not serve.
var ErrUnknownHost = errors.New("unknown host")

// unknownHost carries the message to show while matching ErrUnknownHost.
type unknownHost string

func (e unknownHost) Error() string        { return string(e) }
func (e unknownHost) Is(target error) bool { return target == ErrUnknownHost }

// Connector opens a Manager and returns the func that releases it.
type Connector func() (app.Manager, func(), error)

// RemoteConnector returns the Manager for configured host, authenticating
// with token, and the func that releases it. It must not touch the network:
// the Manager does that on its first call.
type RemoteConnector func(host, token string) (app.Manager, func(), error)

// Host is one entry of the host list.
type Host struct {
	Name  string
	Role  string
	Local bool
}

// Resolver maps host names to Managers. It is immutable; WithRemote and
// WithToken return copies, so a shared resolver can serve concurrent callers
// each with its own token.
type Resolver struct {
	name, role string
	local      Connector
	configured []string // sorted names of the configured remote hosts, never the own name
	ownListed  bool     // the own name is listed under hosts: (it has a daemon)
	remote     RemoteConnector
	token      string
	localCert  string // the local daemon certificate file, for Status
	// defaultPool and hostPools are config.yml's default_pool for the local
	// host and for each remote host: where an empty pool sends a new VM disk.
	defaultPool string
	hostPools   map[string]string
	// thin is the helper client of a hypervisor's thin pools; nil lists none.
	thin *vmtool.Thin
	// builds is the one build-lock table every ThinPools of this resolver (and
	// of its copies) shares; manifestPath is the daemon's image manifest.
	builds       *ThinBuilds
	manifestPath string
}

// New builds the resolver for cfg: the local libvirt and, for every hosts:
// entry, a remote.Manager at https://<address>:<port> (port default 9473)
// that trusts the system store. opts go to each remote.Manager. A nil cfg
// names the local host after the machine's short hostname.
func New(cfg *config.Config, opts ...remote.Option) *Resolver {
	name, role := "", config.RoleWorkstation
	var others []string
	addrs := map[string]string{}
	if cfg != nil {
		name, role = cfg.Name, cfg.Role
		for n, h := range cfg.Hosts {
			others = append(others, n)
			addrs[n] = BaseURL(h)
		}
	}
	if name == "" {
		name, _ = config.ShortHostname()
	}
	if role == "" {
		role = config.RoleWorkstation
	}
	r := NewWith(name, role, others, libvirtLocalNamed(name))
	if role == config.RoleHypervisor {
		r = r.WithThin(&vmtool.Thin{})
	}
	if cfg != nil && cfg.TLSCert != "" {
		r = r.WithLocalCert(cfg.Path(cfg.TLSCert))
	}
	if cfg != nil {
		pools := map[string]string{}
		for n, h := range cfg.Hosts {
			pools[n] = h.DefaultPool
		}
		r = r.WithDefaultPools(cfg.DefaultPool, pools)
	}
	return r.WithRemote(func(host, token string) (app.Manager, func(), error) {
		m, err := remote.New(host, addrs[host], token, opts...)
		if err != nil {
			return nil, nil, err
		}
		return m, m.Close, nil
	})
}

// BaseURL is the daemon URL for a hosts: entry: https://<address>:<port>,
// port defaulting to config.DefaultPort.
func BaseURL(h config.Host) string {
	port := h.Port
	if port == 0 {
		port = config.DefaultPort
	}
	return "https://" + net.JoinHostPort(h.Address, strconv.Itoa(port))
}

// NewWith builds a resolver with an explicit local connector; tests use it to
// prove an unknown host never reaches libvirt. Configured hosts are listed but
// not served until WithRemote supplies their connector.
func NewWith(name, role string, configured []string, local Connector) *Resolver {
	var c []string
	listed := false
	for _, n := range configured {
		if n == name {
			listed = true
			continue
		}
		c = append(c, n)
	}
	sort.Strings(c)
	return &Resolver{name: name, role: role, local: local, configured: c, ownListed: listed}
}

// WithRemote returns a copy of r that serves its configured hosts with rc.
func (r *Resolver) WithRemote(rc RemoteConnector) *Resolver {
	c := *r
	c.remote = rc
	return &c
}

// WithThin returns a copy of r whose hypervisor-role local Manager also lists
// the thin pools t reports (see ThinPools). New sets it for a hypervisor;
// without it, or on any other role, no thin pool is listed.
func (r *Resolver) WithThin(t *vmtool.Thin) *Resolver {
	c := *r
	c.thin = t
	if c.builds == nil {
		c.builds = &ThinBuilds{}
	}
	return &c
}

// WithImageManifest returns a copy of r whose thin-pool clones read the
// daemon's image manifest at path (empty: vmtool.ImageManifestFile in the
// working directory, which is what the server reads too).
func (r *Resolver) WithImageManifest(path string) *Resolver {
	c := *r
	c.manifestPath = path
	return &c
}

// CleanThinBuilding removes the half-built template volumes a stopped daemon
// left (see CleanThinBuilding); a resolver without a helper, or not of the
// hypervisor role, does nothing.
func (r *Resolver) CleanThinBuilding(log *slog.Logger) int {
	if r.thin == nil || r.role != config.RoleHypervisor {
		return 0
	}
	return CleanThinBuilding(r.thin, log)
}

// ExpireExports removes the export volumes that are older than
// vmtool.ExportTTL at now (see ThinPools.ExpireExports); a resolver without a
// helper, or not of the hypervisor role, does nothing. It logs only failures.
func (r *Resolver) ExpireExports(now time.Time, log *slog.Logger) int {
	if r.thin == nil || r.role != config.RoleHypervisor {
		return 0
	}
	return ThinPools{Thin: r.thin, Builds: r.builds, Log: log}.ExpireExports(now)
}

// WithToken returns a copy of r whose remote Managers authenticate with
// token. r itself is unchanged.
func (r *Resolver) WithToken(token string) *Resolver {
	c := *r
	c.token = token
	return &c
}

// libvirtLocalNamed connects to local libvirt; the Manager knows the name
// this server calls itself, for errors that tell the user what to run on it
// (no OVMF installed).
func libvirtLocalNamed(name string) Connector {
	return func() (app.Manager, func(), error) {
		m, err := vmtool.NewManager()
		if err != nil {
			return nil, nil, err
		}
		m.SetHostName(name)
		return m, func() { m.Close() }, nil
	}
}

// WithDefaultPools returns a copy of r that sends an empty pool of a new VM
// disk to local on the local host and to remote[host] on a remote one (an
// empty or missing entry changes nothing). See defaultPools.
func (r *Resolver) WithDefaultPools(local string, remote map[string]string) *Resolver {
	c := *r
	c.defaultPool = local
	c.hostPools = remote
	return &c
}

// Local is the name this server calls itself.
func (r *Resolver) Local() string { return r.name }

// OwnListed reports whether the own name is listed under hosts: in config.yml,
// that is, whether this machine has a daemon. When true every operation on the
// own name goes to that daemon and local libvirt is never opened; when false
// local libvirt is the own name's backend, opened lazily by Resolve(Local()).
// It reads configuration only: nothing is opened or probed.
func (r *Resolver) OwnListed() bool { return r.ownListed }

// Names lists the hosts Resolve serves: the local host, then the configured
// hosts in name order when a remote connector serves them.
func (r *Resolver) Names() []string {
	if r.remote == nil {
		return []string{r.name}
	}
	return r.AllNames()
}

// AllNames is the local host followed by the configured hosts in name order:
// every name that owns a .machines/<host>/ directory (app.Service.HostNames).
// A listed own name appears once, in the first place.
func (r *Resolver) AllNames() []string {
	return append([]string{r.name}, r.configured...)
}

// IsRemote reports whether host is served by a remote daemon: one of the
// configured hosts, or the own name when it is listed.
func (r *Resolver) IsRemote(host string) bool {
	if host == r.name {
		return r.ownListed
	}
	i := sort.SearchStrings(r.configured, host)
	return i < len(r.configured) && r.configured[i] == host
}

// ParseVM parses a host/vm argument, suggesting the local host for a bare
// name. It does not check that the host is served; Resolve does.
func (r *Resolver) ParseVM(s string) (app.VMRef, error) {
	return app.ParseVMRef(s, r.name)
}

// List is the own name followed by the configured hosts in name order. A
// listed own name is a hypervisor row, not a local one.
func (r *Resolver) List() []Host {
	out := []Host{{Name: r.name, Role: r.role, Local: true}}
	if r.ownListed {
		out[0] = Host{Name: r.name, Role: config.RoleHypervisor}
	}
	for _, n := range r.configured {
		out = append(out, Host{Name: n, Role: config.RoleHypervisor})
	}
	return out
}

// Resolve returns the Manager for host and the func that releases it. The
// local host opens libvirt; a configured host gets a remote client with the
// resolver's token (an error, without building one, if there is none). Any
// other host wraps ErrUnknownHost. Neither failure touches libvirt or the
// network.
func (r *Resolver) Resolve(host string) (app.Manager, func(), error) {
	if err := r.Check(host); err != nil {
		return nil, nil, err
	}
	if host == r.name && !r.ownListed {
		m, done, err := r.local()
		if err != nil && r.role != config.RoleHypervisor {
			err = fmt.Errorf("no daemon listed for %s and no local libvirt: %w", r.name, err)
		}
		if err == nil && m != nil && r.role == config.RoleHypervisor {
			if r.thin != nil {
				m = ThinPools{Manager: m, Thin: r.thin, Builds: r.builds, ManifestPath: r.manifestPath}
			}
			m = hypervisorPools{m}
		}
		if err == nil && m != nil && r.defaultPool != "" {
			m = withDefaultPools(m, r.defaultPool)
		}
		return m, done, err
	}
	if r.token == "" {
		return nil, nil, fmt.Errorf("host %q: no token to authenticate to its daemon", host)
	}
	m, done, err := r.remote(host, r.token)
	if p := r.hostPools[host]; err == nil && m != nil && p != "" {
		m = withDefaultPools(m, p)
	}
	return m, done, err
}

// Check reports whether Resolve serves host, without building a Manager: an
// error wrapping ErrUnknownHost for a host this resolver does not serve.
func (r *Resolver) Check(host string) error {
	if host == r.name && !r.ownListed {
		return nil
	}
	if !r.IsRemote(host) {
		return unknownHost(fmt.Sprintf("host %q is not served by %s; known hosts: %s",
			host, r.name, strings.Join(r.Names(), ", ")))
	}
	if r.remote == nil {
		return unknownHost(fmt.Sprintf("host %q is configured but not served by this server (served: %s)",
			host, strings.Join(r.Names(), ", ")))
	}
	return nil
}

// WithLocalCert returns a copy of r whose local Status row reports the
// NotAfter of the first certificate in the PEM file path.
func (r *Resolver) WithLocalCert(path string) *Resolver {
	c := *r
	c.localCert = path
	return &c
}

// Status is one app.HostStatus per host: the local row (always reachable,
// this binary's API version, address "local", the certificate from
// WithLocalCert when set) and then every configured host probed
// concurrently under timeout with the resolver's token (see app.ProbeAll).
// A resolver without a remote connector, or without a token, reports its
// configured hosts as unreachable with the reason; nothing is stored or
// logged.
func (r *Resolver) Status(ctx context.Context, timeout time.Duration) []app.HostStatus {
	names := r.configured
	var out []app.HostStatus
	if r.ownListed {
		names = append([]string{r.name}, names...)
	} else {
		out = append(out, r.localStatus())
	}
	probed := app.ProbeAll(ctx, names, r.Resolve, timeout)
	for i := range probed {
		probed[i].Role = config.RoleHypervisor
	}
	return append(out, probed...)
}

func (r *Resolver) localStatus() app.HostStatus {
	st := app.HostStatus{Host: r.name, Role: r.role, Local: true, Address: "local", Reachable: true}
	// Not listed: local libvirt is the own name's backend, so the row learns
	// whether it opens by opening it once and releasing it at once. A
	// hypervisor daemon's row stays unconditional, as before.
	if r.role != config.RoleHypervisor {
		_, done, err := r.local()
		if err != nil {
			st.Reachable = false
			st.Err = fmt.Errorf("no daemon listed for %s and no local libvirt: %w", r.name, err)
			return st
		}
		if done != nil {
			done()
		}
	}
	v, err := remote.OurVersion()
	if err != nil {
		st.Err = err
		return st
	}
	st.APIVersion = v
	if r.localCert != "" {
		st.CertNotAfter, err = certNotAfter(r.localCert)
		if err != nil {
			st.Err = err
		}
	}
	return st
}

// certNotAfter is the NotAfter of the first certificate in the PEM file: the
// leaf, in the chain files deploy writes.
func certNotAfter(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("read tls_cert: %w", err)
	}
	for {
		var blk *pem.Block
		blk, data = pem.Decode(data)
		if blk == nil {
			return time.Time{}, fmt.Errorf("tls_cert %s: no certificate found", path)
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return time.Time{}, fmt.Errorf("tls_cert %s: %w", path, err)
		}
		return c.NotAfter, nil
	}
}

// hypervisorPools is a hypervisor daemon's local Manager: an empty pool, which
// every surface sends when the user names none, is the hypervisor's own
// default pool vmtool.HypervisorPool instead of libvirt's "default" (which
// the libvirt Manager would use, and which a shared hypervisor may not have).
// A named pool passes through unchanged.
type hypervisorPools struct{ app.Manager }

func hvPool(pool string) string {
	if pool == "" {
		return vmtool.HypervisorPool
	}
	return pool
}

func (h hypervisorPools) MigrateDisk(name, pool string) error {
	return h.Manager.MigrateDisk(name, hvPool(pool))
}

func (h hypervisorPools) CreateVolume(pool, name string, sizeGB uint) (string, error) {
	return h.Manager.CreateVolume(hvPool(pool), name, sizeGB)
}

func (h hypervisorPools) AddDisk(name string, sizeGB uint, pool string) (*vmtool.DiskDevice, error) {
	return h.Manager.AddDisk(name, sizeGB, hvPool(pool))
}

func (h hypervisorPools) DeleteImage(name, pool string) error {
	return h.Manager.DeleteImage(name, hvPool(pool))
}

func (h hypervisorPools) CloneImage(basePath, newName, targetPool string, sizeBytes uint64) (string, error) {
	return h.Manager.CloneImage(basePath, newName, hvPool(targetPool), sizeBytes)
}

// CloneImageFrom keeps app.TemplateCloner visible through the wrapper (a
// thin-pool Manager inside needs the image name); over a Manager without it,
// it is CloneImage.
func (h hypervisorPools) CloneImageFrom(image, basePath, newName, targetPool string, sizeBytes uint64) (string, error) {
	if tc, ok := h.Manager.(app.TemplateCloner); ok {
		return tc.CloneImageFrom(image, basePath, newName, hvPool(targetPool), sizeBytes)
	}
	return h.Manager.CloneImage(basePath, newName, hvPool(targetPool), sizeBytes)
}

// ThinVolume and SavedTemplatePath keep app.ThinHost visible; over a Manager
// without thin pools nothing is a thin volume.
func (h hypervisorPools) ThinVolume(path string) (string, bool, error) {
	return thinVolumeOf(h.Manager, path)
}

func (h hypervisorPools) SavedTemplatePath(name string) (string, error) {
	return savedTemplatePathOf(h.Manager, name)
}

func thinVolumeOf(m app.Manager, path string) (string, bool, error) {
	if th, ok := m.(app.ThinHost); ok {
		return th.ThinVolume(path)
	}
	return "", false, nil
}

func savedTemplatePathOf(m app.Manager, name string) (string, error) {
	if th, ok := m.(app.ThinHost); ok {
		return th.SavedTemplatePath(name)
	}
	return "", fmt.Errorf("template %q not found: %w", name, vmtool.ErrNotFound)
}

// Define fills Pool only; an empty ExtraDiskPool means "same as Pool".
func (h hypervisorPools) Define(cfg vmtool.VMConfig) error {
	cfg.Pool = hvPool(cfg.Pool)
	return h.Manager.Define(cfg)
}

// RefreshPool keeps the optional interface the image cache uses
// (internal/server refreshPool) visible through the wrapper.
func (h hypervisorPools) RefreshPool(pool string) error {
	if r, ok := h.Manager.(interface{ RefreshPool(string) error }); ok {
		return r.RefreshPool(hvPool(pool))
	}
	return nil
}

// defaultPools applies a host's config.yml default_pool: an empty pool becomes
// pool for the operations that put a new VM disk somewhere (CloneImage target,
// CreateVolume, AddDisk, Define's Pool). Everything else (images, ListPools,
// MigrateDisk, resize, RefreshPool) is untouched, so on a hypervisor, where it
// wraps hypervisorPools, the image cache keeps its vmtool default. Define's
// ExtraDiskPool stays as sent: empty already means "same as Pool".
type defaultPools struct {
	app.Manager
	pool string
}

func (d defaultPools) or(pool string) string {
	if pool == "" {
		return d.pool
	}
	return pool
}

func (d defaultPools) CreateVolume(pool, name string, sizeGB uint) (string, error) {
	return d.Manager.CreateVolume(d.or(pool), name, sizeGB)
}

func (d defaultPools) AddDisk(name string, sizeGB uint, pool string) (*vmtool.DiskDevice, error) {
	return d.Manager.AddDisk(name, sizeGB, d.or(pool))
}

func (d defaultPools) CloneImage(basePath, newName, targetPool string, sizeBytes uint64) (string, error) {
	return d.Manager.CloneImage(basePath, newName, d.or(targetPool), sizeBytes)
}

// CloneImageFrom is CloneImage with the image name (see hypervisorPools).
func (d defaultPools) CloneImageFrom(image, basePath, newName, targetPool string, sizeBytes uint64) (string, error) {
	if tc, ok := d.Manager.(app.TemplateCloner); ok {
		return tc.CloneImageFrom(image, basePath, newName, d.or(targetPool), sizeBytes)
	}
	return d.Manager.CloneImage(basePath, newName, d.or(targetPool), sizeBytes)
}

// ThinVolume and SavedTemplatePath keep app.ThinHost visible.
func (d defaultPools) ThinVolume(path string) (string, bool, error) {
	return thinVolumeOf(d.Manager, path)
}

func (d defaultPools) SavedTemplatePath(name string) (string, error) {
	return savedTemplatePathOf(d.Manager, name)
}

// TemplatePath keeps app.TemplateFinder visible, so create on a listed host
// with a default_pool can still use that host's saved template; a Manager that
// cannot ask has none.
func (d defaultPools) TemplatePath(name string) (string, error) {
	if tf, ok := d.Manager.(app.TemplateFinder); ok {
		return tf.TemplatePath(name)
	}
	return "", fmt.Errorf("template %q not found: %w", name, vmtool.ErrNotFound)
}

func (d defaultPools) Define(cfg vmtool.VMConfig) error {
	cfg.Pool = d.or(cfg.Pool)
	return d.Manager.Define(cfg)
}

// RefreshPools keeps the workstation's pool rescan (C8) visible to
// app.FindLocalImage. A Manager without it (a remote host, or
// hypervisorPools on a daemon, which never refreshes) gets a no-op; the
// daemon calls FindLocalImage with refresh off, so it is never used there.
func (d defaultPools) RefreshPools() error {
	if r, ok := d.Manager.(interface{ RefreshPools() error }); ok {
		return r.RefreshPools()
	}
	return nil
}

// RefreshPool keeps the optional interface the image cache uses visible.
func (d defaultPools) RefreshPool(pool string) error {
	if r, ok := d.Manager.(interface{ RefreshPool(string) error }); ok {
		return r.RefreshPool(pool)
	}
	return nil
}

// Probe keeps app.Prober visible through the wrapper, so a remote host with a
// default_pool is still probed by Status and `vmtool hosts`. A Manager that
// cannot probe reports so, as app.ProbeAll does.
func (d defaultPools) Probe(ctx context.Context) app.HostStatus {
	if p, ok := d.Manager.(app.Prober); ok {
		return p.Probe(ctx)
	}
	return app.HostStatus{Err: errors.New("host cannot be probed")}
}

// storeDefaultPools is a defaultPools over a Manager that caches images: the
// wrapper keeps app.ImageStore visible, so create still syncs the image to a
// listed host that has a default_pool.
type storeDefaultPools struct {
	defaultPools
	app.ImageStore
}

// fetchDefaultPools also keeps app.ImageFetcher visible.
type fetchDefaultPools struct {
	storeDefaultPools
	app.ImageFetcher
}

// withDefaultPools wraps m in defaultPools and keeps the optional image
// interfaces m has (and only those: a wrapper must not claim a capability the
// wrapped Manager lacks, or create would treat a local libvirt host as a host
// with an image cache).
func withDefaultPools(m app.Manager, pool string) app.Manager {
	d := defaultPools{m, pool}
	st, ok := m.(app.ImageStore)
	if !ok {
		return d
	}
	sd := storeDefaultPools{d, st}
	if f, ok := m.(app.ImageFetcher); ok {
		return fetchDefaultPools{sd, f}
	}
	return sd
}
