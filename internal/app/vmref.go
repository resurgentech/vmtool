package app

import (
	"fmt"
	"strings"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// VMRef names a VM as host/vm. The host is a vmtool host name (the local
// host's hostname or a configured hosts: key); the Manager for that host
// only ever sees Name.
type VMRef struct {
	Host string
	Name string
}

func (r VMRef) String() string { return r.Host + "/" + r.Name }

// ParseVMRef parses "host/vm". Both parts must be valid machine names. A bare
// name is refused with a suggestion naming local, the local host. Whether
// the host is served is the host resolver's call, not the parser's.
func ParseVMRef(s, local string) (VMRef, error) {
	host, name, ok := strings.Cut(s, "/")
	if !ok {
		if vmtool.ValidMachineName(s) {
			return VMRef{}, invalid("", fmt.Errorf("%q: VM names are host/vm; did you mean %s/%s?", s, local, s))
		}
		return VMRef{}, invalid("", fmt.Errorf("%q: VM names are host/vm", s))
	}
	if !vmtool.ValidMachineName(host) || !vmtool.ValidMachineName(name) {
		return VMRef{}, invalid("", fmt.Errorf("%q: want host/vm, each letters, digits, '.', '_', '-'", s))
	}
	return VMRef{Host: host, Name: name}, nil
}

func validRef(op string, ref VMRef) error {
	if !vmtool.ValidMachineName(ref.Host) || !vmtool.ValidMachineName(ref.Name) {
		return invalid(op, fmt.Errorf("invalid VM %q: want host/vm", ref.String()))
	}
	return nil
}
