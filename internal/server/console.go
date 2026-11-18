package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	"github.com/jaredeh/vmtool/internal/api"
	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// GET /v1/hosts/{host}/vms/{name}/console and GET /v1/metal/{name}/console:
// a WebSocket to the login shell of a VM or machine (ssh -tt on a local
// pty). See the operations' descriptions in spec/openapi/vmtool.yaml for the
// contract. No stream byte, control message or typed input is ever logged or
// put in an error.
const (
	consoleProtocol = "websocket"
	consoleTerm     = "xterm-256color"
	defaultCols     = 80
	defaultRows     = 24
	// maxConsoles is how many consoles one server runs at once. Each is an
	// ssh, a pty and a tunnel process.
	maxConsoles = 16
	// consoleMaxMsg is the largest inbound message; a larger one closes the
	// socket with 1009.
	consoleMaxMsg = 64 << 10
	// consoleChunk is the largest output message.
	consoleChunk = 32 << 10
	// defaultPing is how often the server pings, and defaultPong how long it
	// waits for the pong, before it ends the console.
	defaultPing = 30 * time.Second
	defaultPong = 30 * time.Second
)

// consoles is the console handler's state and limits. Zero limits mean the
// defaults above; tests shorten them.
type consoles struct {
	max       int
	pingEvery time.Duration
	pongWait  time.Duration

	mu     sync.Mutex
	active map[*console]struct{}
	closed bool
}

// console is one open console as the registry sees it. end carries the
// first reason it ends; runConsole acts on it.
type console struct {
	end chan ending
}

// finish records why the console ends, unless it already has a reason.
func (c *console) finish(e ending) {
	select {
	case c.end <- e:
	default:
	}
}

// OpenVMConsole on the strict interface is never reached: tunnelMux serves
// the route (a strict handler cannot take over the connection).
func (h *handlers) OpenVMConsole(_ context.Context, _ api.OpenVMConsoleRequestObject) (api.OpenVMConsoleResponseObject, error) {
	return api.OpenVMConsole500JSONResponse{Error: "openVMConsole needs a connection upgrade; served by tunnelMux"}, nil
}

// OpenMachineConsole: see OpenVMConsole.
func (h *handlers) OpenMachineConsole(_ context.Context, _ api.OpenMachineConsoleRequestObject) (api.OpenMachineConsoleResponseObject, error) {
	return api.OpenMachineConsole500JSONResponse{Error: "openMachineConsole needs a connection upgrade; served by tunnelMux"}, nil
}

func (t tunnelMux) OpenVMConsole(w http.ResponseWriter, r *http.Request, host api.HostName, name api.VMName, params api.OpenVMConsoleParams) {
	setNote(r.Context(), "openVMConsole", r)
	ref := app.VMRef{Host: host, Name: name}
	t.h.serveConsole(w, r, name, params.Cols, params.Rows, func(ctx context.Context) (*exec.Cmd, error) {
		v, err := t.h.withManager(ctx, host, func(m app.Manager) (any, error) {
			return t.h.guestSvc(ctx).ConsoleVM(ctx, m, ref)
		})
		if err != nil {
			return nil, err
		}
		return v.(*exec.Cmd), nil
	})
}

func (t tunnelMux) OpenMachineConsole(w http.ResponseWriter, r *http.Request, name api.MachineName, params api.OpenMachineConsoleParams) {
	setNote(r.Context(), "openMachineConsole", r)
	t.h.serveConsole(w, r, name, params.Cols, params.Rows, func(context.Context) (*exec.Cmd, error) {
		return t.h.svc.ConsoleMachine(name)
	})
}

// refuse sends a JSON Error before any upgrade.
func refuse(w http.ResponseWriter, status int, msg string, header ...string) {
	w.Header().Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		w.Header().Set(header[i], header[i+1])
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Error{Error: msg})
}

// consoleSize parses an optional cols/rows value: an integer in 1-65535.
func consoleSize(p *string, def int) (uint16, bool) {
	if p == nil {
		return uint16(def), true
	}
	n, err := strconv.ParseUint(*p, 10, 16)
	if err != nil || n < 1 {
		return 0, false
	}
	return uint16(n), true
}

// serveConsole runs the handshake checks in the contract's order (the bearer
// and workstation checks already ran in front of the mux), starts ssh on a
// pty, and only then upgrades, so every refusal is an ordinary JSON response.
// It returns when the console ends; the request log's status is 101.
func (h *handlers) serveConsole(w http.ResponseWriter, r *http.Request, name string, qcols, qrows *string, build func(context.Context) (*exec.Cmd, error)) {
	// 3. Browser access.
	if len(r.Header.Values("Origin")) > 0 {
		refuse(w, http.StatusForbidden, "browser access is not allowed")
		return
	}
	// 4. HTTP/1.1 only: HTTP/2 cannot upgrade a connection.
	if r.ProtoMajor != 1 {
		refuse(w, http.StatusUpgradeRequired,
			fmt.Sprintf("the console needs HTTP/1.1 (got HTTP/%d.%d, which cannot upgrade a connection)", r.ProtoMajor, r.ProtoMinor),
			"Upgrade", consoleProtocol)
		return
	}
	// 5. A WebSocket upgrade.
	if !headerHasToken(r.Header, "Connection", "upgrade") || !headerHasToken(r.Header, "Upgrade", consoleProtocol) ||
		r.Header.Get("Sec-WebSocket-Version") != "13" || r.Header.Get("Sec-WebSocket-Key") == "" {
		refuse(w, http.StatusBadRequest, "the console needs a WebSocket upgrade: Connection: Upgrade, Upgrade: websocket, Sec-WebSocket-Version: 13 and Sec-WebSocket-Key")
		return
	}
	// 6. Name, cols and rows.
	if !vmtool.ValidMachineName(name) {
		refuse(w, http.StatusBadRequest, fmt.Sprintf("invalid name %q", name))
		return
	}
	cols, okc := consoleSize(qcols, defaultCols)
	rows, okr := consoleSize(qrows, defaultRows)
	if !okc || !okr {
		refuse(w, http.StatusBadRequest, "cols and rows must be integers from 1 to 65535")
		return
	}
	// 7, 8. The target, and the address of a VM.
	cmd, err := build(r.Context())
	if err != nil {
		switch kindOf(err) {
		case app.KindInvalid:
			refuse(w, http.StatusBadRequest, err.Error())
		case app.KindNotFound:
			refuse(w, http.StatusNotFound, err.Error())
		case app.KindConflict:
			refuse(w, http.StatusConflict, err.Error())
		case app.KindBadGateway:
			refuse(w, http.StatusBadGateway, err.Error())
		default:
			refuse(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// 9. The console limit.
	con := &console{end: make(chan ending, 1)}
	if !h.console.add(con) {
		msg := fmt.Sprintf("too many consoles open (limit %d)", h.console.limit())
		if h.console.isClosed() {
			msg = "the server is shutting down"
		}
		refuse(w, http.StatusTooManyRequests, msg)
		return
	}
	defer h.console.remove(con)
	// 10. ssh on a local pty, started before the upgrade.
	cmd.Env = append(cmd.Environ(), "TERM="+consoleTerm)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		refuse(w, http.StatusBadGateway, "could not start ssh on a terminal: "+err.Error())
		return
	}
	// The server's own deadlines must not end a console.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		// Accept answered the client itself.
		_ = ptmx.Close()
		killGroup(cmd)
		_ = cmd.Wait()
		return
	}
	h.runConsole(conn, ptmx, cmd, con)
}

// killGroup SIGKILLs the shell's whole process group (the child is a session
// leader, so its pid is the group id): ssh and the tunnel's ProxyCommand.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

func (c *consoles) limit() int {
	if c.max > 0 {
		return c.max
	}
	return maxConsoles
}

// add registers con unless the limit is reached or the server is shutting
// down.
func (c *consoles) add(con *console) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || len(c.active) >= c.limit() {
		return false
	}
	if c.active == nil {
		c.active = map[*console]struct{}{}
	}
	c.active[con] = struct{}{}
	return true
}

func (c *consoles) remove(con *console) {
	c.mu.Lock()
	delete(c.active, con)
	c.mu.Unlock()
}

// ending is how a console ends: the close frame to send (code 0 means none:
// the client closed or the connection dropped).
type ending struct {
	code   websocket.StatusCode
	reason string
}

// drainWait bounds how long the last output may take to reach the client
// after the shell exited.
const drainWait = 3 * time.Second

// runConsole relays between the socket and the pty until one side ends, then
// kills the shell's process group, waits for it and closes the socket. One
// context ends the relay goroutines together.
func (h *handlers) runConsole(conn *websocket.Conn, ptmx *os.File, cmd *exec.Cmd, con *console) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	// The size limit is ours (consoleMaxMsg), so the close reason is ours.
	conn.SetReadLimit(-1)

	finish := con.finish

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	// Output: pty to socket, in chunks of at most consoleChunk.
	outDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(outDone)
		buf := make([]byte, consoleChunk)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if werr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					finish(ending{})
					return
				}
			}
			if err != nil {
				// EIO once every process holding the terminal is gone: the
				// normal end of output. The exit path below decides what it
				// means.
				return
			}
		}
	}()

	// Input: binary messages to the pty, text messages are controls.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			typ, r, err := conn.Reader(ctx)
			if err != nil {
				finish(ending{})
				return
			}
			b, err := io.ReadAll(io.LimitReader(r, consoleMaxMsg+1))
			if err != nil {
				finish(ending{})
				return
			}
			if len(b) > consoleMaxMsg {
				finish(ending{websocket.StatusMessageTooBig, "message too big"})
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if _, err := ptmx.Write(b); err != nil {
					finish(ending{websocket.StatusInternalError, "console failed"})
					return
				}
			case websocket.MessageText:
				// A bad control message is ignored: not written to the
				// terminal, nothing logged.
				if cols, rows, ok := parseResize(b); ok {
					_ = pty.Setsize(ptmx, &pty.Winsize{Cols: cols, Rows: rows})
				}
			}
		}
	}()

	// Ping: with no pong in pongWait the console ends.
	wg.Add(1)
	go func() {
		defer wg.Done()
		every, wait := h.console.pingEvery, h.console.pongWait
		if every <= 0 {
			every = defaultPing
		}
		if wait <= 0 {
			wait = defaultPong
		}
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			pctx, pcancel := context.WithTimeout(ctx, wait)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				if ctx.Err() == nil {
					finish(ending{websocket.StatusInternalError, "ping timeout"})
				}
				return
			}
		}
	}()

	select {
	case <-exited:
		// The shell is done. Kill anything it left in its group (that also
		// closes the terminal's last holders), let the last output reach the
		// client, then say how it ended.
		killGroup(cmd)
		select {
		case <-outDone:
		case <-time.After(drainWait):
		}
		_ = ptmx.Close()
		code, reason := websocket.StatusInternalError, "console failed"
		if n := cmd.ProcessState.ExitCode(); n >= 0 {
			code, reason = statusExit, "exit "+strconv.Itoa(n)
		}
		_ = conn.Close(code, reason)
	case e := <-con.end:
		killGroup(cmd)
		_ = ptmx.Close()
		<-exited
		if e.code != 0 {
			_ = conn.Close(e.code, e.reason)
		} else {
			conn.CloseNow()
		}
	}
}

// statusExit closes a console whose shell exited; the reason is "exit N".
const statusExit websocket.StatusCode = 4000

// parseResize reads {"event":"resize","cols":N,"rows":N}. Anything else
// (not JSON, another event, a size that is not an integer in 1-65535) is not
// a resize.
func parseResize(b []byte) (cols, rows uint16, ok bool) {
	var m struct {
		Event string `json:"event"`
		Cols  *int   `json:"cols"`
		Rows  *int   `json:"rows"`
	}
	if json.Unmarshal(b, &m) != nil || m.Event != "resize" || m.Cols == nil || m.Rows == nil {
		return 0, 0, false
	}
	if *m.Cols < 1 || *m.Cols > 65535 || *m.Rows < 1 || *m.Rows > 65535 {
		return 0, 0, false
	}
	return uint16(*m.Cols), uint16(*m.Rows), true
}

// attachShutdown makes srv's Shutdown end every console with 4503. A hijacked
// connection is not tracked by http.Server, so without this a shutdown would
// leave each console's ssh running.
func (h *handlers) attachShutdown(srv *http.Server) {
	srv.RegisterOnShutdown(h.console.shutdown)
}

// shutdown ends every open console with 4503 "unavailable" and refuses new
// ones (a request already past the listener when the server stops).
func (c *consoles) shutdown() {
	c.mu.Lock()
	c.closed = true
	all := make([]*console, 0, len(c.active))
	for con := range c.active {
		all = append(all, con)
	}
	c.mu.Unlock()
	for _, con := range all {
		con.finish(ending{websocket.StatusCode(4503), "unavailable"})
	}
}

func (c *consoles) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
