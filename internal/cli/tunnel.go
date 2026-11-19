package cli

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/remote"
	"github.com/jaredeh/vmtool/pkg/vmtool"
	"github.com/spf13/cobra"
)

const (
	tunnelGuestPort    = "22"
	tunnelDialTimeout  = 5 * time.Second
	tunnelSetupTimeout = 30 * time.Second
)

// The tunnel's VM lookup, guest dial and TLS trust. Tests replace them so no
// test reaches libvirt, a guest or the system trust store.
var (
	// tunnelVMIP is the IP libvirt reports for a VM on the local host.
	tunnelVMIP = func(e *env, ref app.VMRef) (string, error) {
		r := e.resolver()
		m, release, err := r.Resolve(ref.Host)
		if err != nil {
			return "", err
		}
		defer release()
		info, err := e.service("").GetVM(context.Background(), m, ref.Name)
		if err != nil {
			return "", err
		}
		return info.IP, nil
	}
	// tunnelVMUUID is the domain UUID of a VM, from its host, for the tunnel's
	// credentials fetch. "" is a daemon older than 2.3.0.
	tunnelVMUUID = func(ctx context.Context, e *env, ref app.VMRef) (string, error) {
		r := e.resolver()
		m, release, err := r.Resolve(ref.Host)
		if err != nil {
			return "", err
		}
		defer release()
		info, err := e.service("").GetVM(ctx, m, ref.Name)
		if err != nil {
			return "", err
		}
		return info.UUID, nil
	}
	// tunnelDial connects to a local guest's port 22.
	tunnelDial = func(ctx context.Context, addr string) (net.Conn, error) {
		d := net.Dialer{Timeout: tunnelDialTimeout}
		return d.DialContext(ctx, "tcp", addr)
	}
	// tunnelTLS is the TLS config for a remote daemon; nil RootCAs is the
	// system trust store.
	tunnelTLS *tls.Config
)

func tunnelCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "tunnel <host/vm | vm.host.vmtool>",
		Short: "Pipe stdin/stdout to a VM's SSH port (for ssh ProxyCommand)",
		Long: `Connect stdin and stdout to TCP port 22 of a VM, for use as an SSH
ProxyCommand. stdout carries only the SSH byte stream; errors go to stderr.

On the local host it asks libvirt for the VM's IP and connects directly. For
a host in config.yml's hosts: it opens an authenticated tunnel through that
host's vmtool daemon (HTTPS, HTTP/1.1 Upgrade), using VMTOOL_TOKEN.

In store mode, when the VM's credentials folder .machines/<vm>.<host>.vmtool/
is missing, it is first fetched from the store, so the IdentityFile of the
ssh-config block exists. With the folder there the store is not contacted.
Anything it has to say goes to stderr.

The VM is host/vm, or its SSH name <vm>.<host>.vmtool (what the block from
'vmtool ssh-config' passes). The host name has no dots; the VM name may.

  ssh -o ProxyCommand='vmtool tunnel %n' web.jetfire.vmtool`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// stdout is the byte stream: no usage text, and main prints the
			// error once, on stderr.
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			conn, err := openTunnel(ctx, e, args[0])
			if err != nil {
				return err
			}
			return remote.Pipe(ctx, conn, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

// openTunnel connects to arg's port 22: directly for the local host, through
// the host's daemon for a configured one (the own name too, when it is listed). arg is host/vm, or the VM SSH name
// <vm>.<host>.vmtool that ssh passes (%n). The suffix is matched ignoring
// case so a wrong-case name gets ParseSSHName's message, not a host/vm one.
func openTunnel(ctx context.Context, e *env, arg string) (io.ReadWriteCloser, error) {
	r := e.resolver()
	var ref app.VMRef
	if !strings.Contains(arg, "/") && strings.HasSuffix(strings.ToLower(arg), vmtool.SSHNameSuffix) {
		host, vm, err := vmtool.ParseSSHName(arg)
		if err != nil {
			return nil, err
		}
		ref = app.VMRef{Host: host, Name: vm}
	} else {
		var err error
		if ref, err = r.ParseVM(arg); err != nil {
			return nil, err
		}
	}
	setup, cancel := context.WithTimeout(ctx, tunnelSetupTimeout)
	defer cancel()

	// Store mode, and the credentials folder that ssh-config's IdentityFile
	// names is missing: fetch it before ssh needs it. With the folder present
	// nothing here touches the store. Notices go to stderr; stdout stays the
	// SSH byte stream.
	e.service("").FetchMissingVMCreds(setup, ref, func(ctx context.Context) (string, error) {
		return tunnelVMUUID(ctx, e, ref)
	})

	if ref.Host == r.Local() && !r.OwnListed() {
		ip, err := tunnelVMIP(e, ref)
		if err != nil {
			return nil, err
		}
		if ip == "" {
			return nil, fmt.Errorf("VM %s has no IP address", ref)
		}
		addr := net.ParseIP(ip)
		if addr == nil {
			return nil, fmt.Errorf("VM %s has no usable IP address (libvirt reported %q)", ref, ip)
		}
		target := net.JoinHostPort(addr.String(), tunnelGuestPort)
		c, err := tunnelDial(setup, target)
		if err != nil {
			return nil, fmt.Errorf("connect to %s at %s: %w", ref, target, err)
		}
		return c, nil
	}

	var h config.Host
	ok := false
	if e.cfg != nil {
		h, ok = e.cfg.Hosts[ref.Host]
	}
	if !ok {
		return nil, fmt.Errorf("unknown host %q; known hosts: %s", ref.Host, strings.Join(r.AllNames(), ", "))
	}
	return remote.Dial(setup, hosts.BaseURL(h), ref.Host, ref.Name, e.token, tunnelTLS)
}
