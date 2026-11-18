package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jaredeh/vmtool/internal/config"
)

// Test 9: a client close frame and a dropped connection each kill the shell
// and everything it started.
func TestConsoleDisconnectKillsEverything(t *testing.T) {
	for name, end := range map[string]func(*websocket.Conn){
		"close frame": func(c *websocket.Conn) { c.Close(websocket.StatusNormalClosure, "") },
		"drop":        func(c *websocket.Conn) { c.CloseNow() },
		"close 1001":  func(c *websocket.Conn) { c.Close(websocket.StatusGoingAway, "bye") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newConsoleFix(t, holdBody)
			c := f.mustDial(vmConsole)
			pid, pid2 := f.readPid("pid"), f.readPid("pid2")
			if !alive(pid) || !alive(pid2) {
				t.Fatalf("stub processes not running: %d %d", pid, pid2)
			}
			end(c)
			f.assertGone(pid, pid2)
		})
	}
}

// Test 10: a client that never answers pings loses its console: the shell
// and its children are killed.
func TestConsoleMissedPong(t *testing.T) {
	f := newConsoleFix(t, holdBody, func(h *handlers) {
		h.console.pingEvery = 100 * time.Millisecond
		h.console.pongWait = 100 * time.Millisecond
	})
	// A client that never reads never answers a ping.
	c := f.mustDial(vmConsole)
	pid, pid2 := f.readPid("pid"), f.readPid("pid2")
	f.assertGone(pid, pid2)
	_ = c
}

// A client that is reading answers pings, so a quiet console stays open.
func TestConsoleAnsweredPingsKeepItOpen(t *testing.T) {
	f := newConsoleFix(t, "stty -echo\nwhile read l; do echo \"got:$l\"; done", func(h *handlers) {
		h.console.pingEvery = 50 * time.Millisecond
		h.console.pongWait = 2 * time.Second
	})
	c := f.mustDial(vmConsole)
	pid := f.readPid("pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 8)
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				got <- "ERR " + err.Error()
				return
			}
			got <- string(b)
		}
	}()
	time.Sleep(600 * time.Millisecond) // a dozen pings
	if !alive(pid) {
		t.Fatal("console ended although every ping was answered")
	}
	send(t, c, websocket.MessageBinary, "PLACEHOLDER-after-pings\n")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-got:
			if strings.HasPrefix(m, "ERR") {
				t.Fatalf("console ended: %s", m)
			}
			if strings.Contains(m, "got:PLACEHOLDER-after-pings") {
				c.Close(websocket.StatusNormalClosure, "")
				f.assertGone(pid)
				return
			}
		case <-deadline:
			t.Fatal("no echo after the pings")
		}
	}
}

// Test 11: a server shutdown closes an open console with 4503 and kills it.
func TestConsoleShutdown(t *testing.T) {
	f := newConsoleFix(t, holdBody)
	f.h.attachShutdown(f.srv.Config)
	c := f.mustDial(vmConsole)
	pid, pid2 := f.readPid("pid"), f.readPid("pid2")

	done := make(chan websocket.CloseError, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				var ce websocket.CloseError
				asClose(err, &ce)
				done <- ce
				return
			}
		}
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = f.srv.Config.Shutdown(ctx)
	}()
	select {
	case ce := <-done:
		if ce.Code != 4503 || ce.Reason != "unavailable" {
			t.Fatalf("close %d %q, want 4503 \"unavailable\"", ce.Code, ce.Reason)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("console still open after server shutdown")
	}
	f.assertGone(pid, pid2)
	// A console arriving after shutdown starts nothing.
	f.reset()
	before := f.started()
	hdr := f.auth()
	if _, resp, err := f.dial(vmConsole, hdr); err == nil || (resp != nil && resp.StatusCode == http.StatusSwitchingProtocols) {
		t.Fatal("a console opened after shutdown")
	}
	if n := f.started(); n != before {
		t.Fatalf("ssh started after shutdown (%d -> %d)", before, n)
	}
}

// The real server wires the shutdown hook: Server.httpServer registers it.
func TestHTTPServerShutdownClosesConsoles(t *testing.T) {
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	writeCert(t, dir)
	s := &Server{Listen: "127.0.0.1:0", PlaybookDir: "playbooks", Config: &config.Config{
		Dir: dir, TokenSHA256: config.Hash(testToken), TLSCert: "tls/test.crt", TLSKey: "tls/test.key",
	}}
	srv, err := s.httpServer()
	if err != nil {
		t.Fatal(err)
	}
	if s.h.console.isClosed() {
		t.Fatal("consoles closed before shutdown")
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// RegisterOnShutdown hooks run in their own goroutines.
	deadline := time.Now().Add(5 * time.Second)
	for !s.h.console.isClosed() {
		if time.Now().After(deadline) {
			t.Fatal("httpServer does not close consoles on shutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
