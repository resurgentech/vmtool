// Package hosts resolves a host name to the app.Manager that drives it.
//
// Today only the local host is served: the Manager is the libvirt-backed
// vmtool.Manager, opened per request and closed by the caller. Hosts named in
// config.yml are listed but not yet served; remote hosts plug in here later.
package hosts

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
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

// Host is one entry of the host list.
type Host struct {
	Name  string
	Role  string
	Local bool
}

// Resolver maps host names to Managers.
type Resolver struct {
	name, role string
	local      Connector
	configured []string // sorted names of configured, unserved hosts
}

// New builds the resolver for cfg. A nil cfg names the local host after the
// machine's short hostname.
func New(cfg *config.Config) *Resolver {
	name, role := "", config.RoleWorkstation
	var others []string
	if cfg != nil {
		name, role = cfg.Name, cfg.Role
		for n := range cfg.Hosts {
			others = append(others, n)
		}
	}
	if name == "" {
		name, _ = config.ShortHostname()
	}
	if role == "" {
		role = config.RoleWorkstation
	}
	return NewWith(name, role, others, libvirtLocal)
}

// NewWith builds a resolver with an explicit local connector; tests use it to
// prove an unknown host never reaches libvirt.
func NewWith(name, role string, configured []string, local Connector) *Resolver {
	c := append([]string(nil), configured...)
	sort.Strings(c)
	return &Resolver{name: name, role: role, local: local, configured: c}
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

// Names lists the hosts Resolve serves.
func (r *Resolver) Names() []string { return []string{r.name} }

// AllNames is the local host followed by the configured hosts in name order:
// every name that owns a .machines/<host>/ directory (app.Service.HostNames).
func (r *Resolver) AllNames() []string {
	return append([]string{r.name}, r.configured...)
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

// Resolve returns the Manager for host and the func that releases it. Any
// host but the local one wraps ErrUnknownHost, without touching libvirt.
func (r *Resolver) Resolve(host string) (app.Manager, func(), error) {
	if host == r.name {
		return r.local()
	}
	for _, n := range r.configured {
		if n == host {
			return nil, nil, unknownHost(fmt.Sprintf("host %q is configured but not served by this server yet (served: %s)",
				host, strings.Join(r.Names(), ", ")))
		}
	}
	return nil, nil, unknownHost(fmt.Sprintf("host %q is not served by %s; known hosts: %s",
		host, r.name, strings.Join(r.Names(), ", ")))
}
