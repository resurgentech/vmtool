package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// GET /v1/hosts/{host}/vms/{name}/tunnel: an HTTP/1.1 Upgrade (RFC 9110
// section 7.8) to a raw byte stream to the guest's TCP port 22. See the
// operation's description in spec/openapi/vmtool.yaml for the contract.
const (
	tunnelProtocol   = "vmtool-tunnel"
	guestSSHPort     = "22"
	guestDialTimeout = 5 * time.Second
	tunnel101        = "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + tunnelProtocol + "\r\n\r\n"
)

// tunnelMux is the generated ServerInterface (the strict adapter) with the
// operations that take over the connection replaced: OpenVMTunnel, the two
// consoles (console.go) and uploadImage (images.go). A strict handler returns
// a response object and cannot upgrade or stream. Every other operation goes
// to the strict adapter unchanged. The bearer check, request log and body
// limit wrap this mux like every route (chain in server.go).
type tunnelMux struct {
	api.ServerInterface
	h *handlers
}

func (t tunnelMux) OpenVMTunnel(w http.ResponseWriter, r *http.Request, host api.HostName, name api.VMName) {
	setNote(r.Context(), "openVMTunnel", r)
	t.h.openTunnel(w, r, host, name)
}

// OpenVMTunnel on the strict interface is never reached: tunnelMux serves
// the route. It exists because the generated interface requires it.
func (h *handlers) OpenVMTunnel(_ context.Context, _ api.OpenVMTunnelRequestObject) (api.OpenVMTunnelResponseObject, error) {
	return api.OpenVMTunnel500JSONResponse{Error: "openVMTunnel needs a connection upgrade; served by tunnelMux"}, nil
}

// openTunnel checks the request, resolves the VM, dials <ip>:22 and only
// then hijacks the connection, so every refusal is an ordinary JSON
// response. It returns when the tunnel ends: the request log's duration is
// the session's length and its status is 101 (statusWriter.Hijack).
func (h *handlers) openTunnel(w http.ResponseWriter, r *http.Request, host, name string) {
	if r.ProtoMajor != 1 {
		_ = api.OpenVMTunnel426JSONResponse{
			Body:    api.Error{Error: fmt.Sprintf("the tunnel needs HTTP/1.1 (got HTTP/%d.%d, which cannot upgrade a connection)", r.ProtoMajor, r.ProtoMinor)},
			Headers: api.OpenVMTunnel426ResponseHeaders{Upgrade: tunnelProtocol},
		}.VisitOpenVMTunnelResponse(w)
		return
	}
	if !headerHasToken(r.Header, "Connection", "upgrade") || !headerHasToken(r.Header, "Upgrade", tunnelProtocol) {
		_ = api.OpenVMTunnel400JSONResponse{Error: "the tunnel needs Connection: Upgrade and Upgrade: " + tunnelProtocol}.VisitOpenVMTunnelResponse(w)
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
		_ = api.OpenVMTunnel400JSONResponse{Error: "the tunnel request takes no body"}.VisitOpenVMTunnelResponse(w)
		return
	}
	if !vmtool.ValidMachineName(name) {
		_ = api.OpenVMTunnel400JSONResponse{Error: fmt.Sprintf("invalid VM name %q", name)}.VisitOpenVMTunnelResponse(w)
		return
	}
	// A configured remote host's guests are reachable only from that host:
	// guests on different hosts share NAT subnets, so this server must not
	// look up a remote VM's address and dial it from here. The client
	// tunnels through the remote daemon itself.
	if h.hosts.IsRemote(host) {
		_ = api.OpenVMTunnel404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse{Error: fmt.Sprintf(
			"tunnels to %s are served by that host's daemon, not by %s; use `vmtool tunnel %s/%s`",
			host, h.hosts.Local(), host, name)}}.VisitOpenVMTunnelResponse(w)
		return
	}

	ip, err := h.lookupVMIP(r.Context(), host, name)
	if err != nil {
		if kindOf(err) == app.KindNotFound {
			_ = api.OpenVMTunnel404JSONResponse{ErrorJSONResponse: api.ErrorJSONResponse(apiErr(err))}.VisitOpenVMTunnelResponse(w)
			return
		}
		_ = api.OpenVMTunnel500JSONResponse(apiErr(err)).VisitOpenVMTunnelResponse(w)
		return
	}
	// Only the address libvirt reports, only port 22.
	if ip == "" {
		_ = api.OpenVMTunnel409JSONResponse{Error: fmt.Sprintf("VM %s/%s has no IP address", host, name)}.VisitOpenVMTunnelResponse(w)
		return
	}
	addr := net.ParseIP(ip)
	if addr == nil {
		_ = api.OpenVMTunnel409JSONResponse{Error: fmt.Sprintf("VM %s/%s has no usable IP address (libvirt reported %q)", host, name, ip)}.VisitOpenVMTunnelResponse(w)
		return
	}
	target := net.JoinHostPort(addr.String(), guestSSHPort)
	guest, err := h.dialGuestSSH(r.Context(), target)
	if err != nil {
		_ = api.OpenVMTunnel502JSONResponse{Error: fmt.Sprintf("connect to %s/%s at %s: %v", host, name, target, err)}.VisitOpenVMTunnelResponse(w)
		return
	}

	// http.ResponseController unwraps to statusWriter, whose Hijack records 101.
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		guest.Close()
		_ = api.OpenVMTunnel500JSONResponse{Error: "connection upgrade not supported: " + err.Error()}.VisitOpenVMTunnelResponse(w)
		return
	}
	relay(conn, rw.Reader, guest)
}

// lookupVMIP is the IP libvirt reports for host/name: through the host
// resolver (unknown host is NotFound without touching libvirt) and the
// workflow layer's GetVM. Tests replace it with h.vmIP.
func (h *handlers) lookupVMIP(ctx context.Context, host, name string) (string, error) {
	if h.vmIP != nil {
		return h.vmIP(ctx, host, name)
	}
	v, err := h.withManager(ctx, host, func(m app.Manager) (any, error) {
		return h.svc.GetVM(ctx, m, name)
	})
	if err != nil {
		return "", err
	}
	info, _ := v.(*vmtool.VMInfo)
	if info == nil {
		return "", nil
	}
	return info.IP, nil
}

func (h *handlers) dialGuestSSH(ctx context.Context, addr string) (net.Conn, error) {
	if h.dialGuest != nil {
		return h.dialGuest(ctx, addr)
	}
	d := net.Dialer{Timeout: guestDialTimeout}
	return d.DialContext(ctx, "tcp", addr)
}

// relay writes the 101, hands the guest any bytes the server already read
// past the request headers, then copies both ways. When either direction
// ends it closes both connections, which ends the other copy; it returns
// once both copies have returned.
func relay(client net.Conn, buffered *bufio.Reader, guest net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			client.Close()
			guest.Close()
		})
	}
	defer closeBoth()

	// The server's ReadHeaderTimeout/ReadTimeout/WriteTimeout deadlines must
	// not end an SSH session.
	_ = client.SetDeadline(time.Time{})

	if _, err := io.WriteString(client, tunnel101); err != nil {
		return
	}
	if n := buffered.Buffered(); n > 0 {
		b, _ := buffered.Peek(n)
		if _, err := guest.Write(b); err != nil {
			return
		}
		_, _ = buffered.Discard(n)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(guest, client)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, guest)
		closeBoth()
	}()
	wg.Wait()
}

// headerHasToken reports whether any comma-separated element of header name
// equals token, ignoring case (RFC 9110 Connection and Upgrade lists).
func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, el := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(el), token) {
				return true
			}
		}
	}
	return false
}
