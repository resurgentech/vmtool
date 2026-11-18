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
	configured []string // sorted names of the configured remote hosts
	remote     RemoteConnector
	token      string
	localCert  string // the local daemon certificate file, for Status
	// defaultPool and hostPools are config.yml's default_pool for the local
	// host and for each remote host: where an empty pool sends a new VM disk.
	defaultPool string
	hostPools   map[string]string
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
	c := append([]string(nil), configured...)
	sort.Strings(c)
	return &Resolver{name: name, role: role, local: local, configured: c}
}

// WithRemote returns a copy of r that serves its configured hosts with rc.
func (r *Resolver) WithRemote(rc RemoteConnector) *Resolver {
	c := *r
	c.remote = rc
	return &c
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
func (r *Resolver) AllNames() []string {
	return append([]string{r.name}, r.configured...)
}

// IsRemote reports whether host is one of the configured remote hosts.
func (r *Resolver) IsRemote(host string) bool {
	if host == r.name {
		return false
	}
	i := sort.SearchStrings(r.configured, host)
	return i < len(r.configured) && r.configured[i] == host
}

// ParseVM parses a host/vm argument, suggesting the local host for a bare
// name. It does not check that the host is served; Resolve does.
func (r *Resolver) ParseVM(s string) (app.VMRef, error) {
	return app.ParseVMRef(s, r.name)
}

// List is the local host followed by the configured hosts in name order.
func (r *Resolver) List() []Host {
	out := []Host{{Name: r.name, Role: r.role, Local: true}}
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
	if host == r.name {
		m, done, err := r.local()
		if err == nil && m != nil && r.role == config.RoleHypervisor {
			m = hypervisorPools{m}
		}
		if err == nil && m != nil && r.defaultPool != "" {
			m = defaultPools{m, r.defaultPool}
		}
		return m, done, err
	}
	if r.token == "" {
		return nil, nil, fmt.Errorf("host %q: no token to authenticate to its daemon", host)
	}
	m, done, err := r.remote(host, r.token)
	if p := r.hostPools[host]; err == nil && m != nil && p != "" {
		m = defaultPools{m, p}
	}
	return m, done, err
}

// Check reports whether Resolve serves host, without building a Manager: an
// error wrapping ErrUnknownHost for a host this resolver does not serve.
func (r *Resolver) Check(host string) error {
	if host == r.name {
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
	out := []app.HostStatus{r.localStatus()}
	probed := app.ProbeAll(ctx, r.configured, r.Resolve, timeout)
	for i := range probed {
		probed[i].Role = config.RoleHypervisor
	}
	return append(out, probed...)
}

func (r *Resolver) localStatus() app.HostStatus {
	st := app.HostStatus{Host: r.name, Role: r.role, Local: true, Address: "local", Reachable: true}
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
