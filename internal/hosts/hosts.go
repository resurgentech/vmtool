// Package hosts resolves a host name to the app.Manager that drives it.
//
// The local host's Manager is the libvirt-backed vmtool.Manager, opened per
// Resolve and closed by the release func. A host named in config.yml hosts:
// is a remote daemon: its Manager is a remote.Manager (HTTPS client) carrying
// the caller's token, which the resolver gets from WithToken (the CLI's
// verified token, or the bearer of the request a server is serving). No
// token is stored in the resolver a server shares between requests.
package hosts

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

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
	return NewWith(name, role, others, libvirtLocal).WithRemote(func(host, token string) (app.Manager, func(), error) {
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

func libvirtLocal() (app.Manager, func(), error) {
	m, err := vmtool.NewManager()
	if err != nil {
		return nil, nil, err
	}
	return m, func() { m.Close() }, nil
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
	if host == r.name {
		return r.local()
	}
	if !r.IsRemote(host) {
		return nil, nil, unknownHost(fmt.Sprintf("host %q is not served by %s; known hosts: %s",
			host, r.name, strings.Join(r.Names(), ", ")))
	}
	if r.remote == nil {
		return nil, nil, unknownHost(fmt.Sprintf("host %q is configured but not served by this server (served: %s)",
			host, strings.Join(r.Names(), ", ")))
	}
	if r.token == "" {
		return nil, nil, fmt.Errorf("host %q: no token to authenticate to its daemon", host)
	}
	return r.remote(host, r.token)
}
