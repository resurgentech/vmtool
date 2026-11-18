package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// recv reads binary messages until the accumulated output contains want and
// returns everything read so far. A text message from the server, a close or
// a timeout fails the test.
func recv(t *testing.T, c *websocket.Conn, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got strings.Builder
	for !strings.Contains(got.String(), want) {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %q, read %q: %v", want, got.String(), err)
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("server sent a %v message %q; it must send binary only", typ, b)
		}
		if len(b) > 32<<10 {
			t.Fatalf("server sent a %d-byte message, over the 32 KiB chunk", len(b))
		}
		got.Write(b)
	}
	return got.String()
}

func send(t *testing.T, c *websocket.Conn, typ websocket.MessageType, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Write(ctx, typ, []byte(s)); err != nil {
		t.Fatalf("write %v: %v", typ, err)
	}
}

// closeOf reads until the connection ends and returns the close code and
// reason, plus all binary output read on the way.
func closeOf(t *testing.T, c *websocket.Conn) (websocket.StatusCode, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out strings.Builder
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			var ce websocket.CloseError
			if !asClose(err, &ce) {
				t.Fatalf("connection ended without a close frame: %v (output %q)", err, out.String())
			}
			return ce.Code, ce.Reason, out.String()
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("server sent a %v message %q", typ, b)
		}
		out.Write(b)
	}
}

func asClose(err error, ce *websocket.CloseError) bool { return errors.As(err, ce) }

// Tests 6 and 15: input and output through the pty for a VM and a machine;
// bad text messages are ignored and leave no trace.
func TestConsoleInputOutput(t *testing.T) {
	body := "[ -t 0 ] && echo TTY-OK\nstty -echo\nwhile read l; do echo \"got:$l\"; done"
	for _, p := range []string{vmConsole, metalConsole} {
		t.Run(p, func(t *testing.T) {
			f := newConsoleFix(t, body)
			c := f.mustDial(p)
			recv(t, c, "TTY-OK")
			pid := f.readPid("pid")

			send(t, c, websocket.MessageBinary, "PLACEHOLDER-in\n")
			recv(t, c, "got:PLACEHOLDER-in")

			// Text messages that are not valid controls are ignored: not
			// written to the terminal, the console stays open.
			bad := []string{
				"PLACEHOLDER-not json",
				`{"event":"PLACEHOLDER-unknown"}`,
				`{"event":"resize","cols":"PLACEHOLDER","rows":24}`,
				`{"event":"resize","cols":0,"rows":24}`,
				`{"event":"resize","cols":80,"rows":70000}`,
				`{"event":"resize","cols":80.5,"rows":24}`,
				`{"event":"resize","cols":80}`,
			}
			for i, m := range bad {
				send(t, c, websocket.MessageText, m+"\n")
				line := "PLACEHOLDER-line-" + string(rune('a'+i))
				send(t, c, websocket.MessageBinary, line+"\n")
				out := recv(t, c, "got:"+line)
				if strings.Contains(out, "PLACEHOLDER-not") || strings.Contains(out, "PLACEHOLDER-unknown") || strings.Contains(out, "resize") {
					t.Fatalf("a text message reached the terminal: %q", out)
				}
			}

			// Control bytes go through as typed input.
			send(t, c, websocket.MessageBinary, "\x04")
			code, reason, _ := closeOf(t, c)
			if code != 4000 || reason != "exit 0" {
				t.Fatalf("after ^D: close %d %q, want 4000 \"exit 0\"", code, reason)
			}
			f.assertGone(pid)
			if strings.Contains(f.log.String(), "PLACEHOLDER") {
				t.Fatalf("console content reached the log:\n%s", f.log)
			}
		})
	}
}

// Test 7: the pty has the requested size, and a resize reaches it.
func TestConsoleResize(t *testing.T) {
	body := "stty -echo\nstty size\nwhile read l; do stty size; done"
	f := newConsoleFix(t, body)
	c := f.mustDial(vmConsole + "?cols=100&rows=40")
	recv(t, c, "40 100")
	pid := f.readPid("pid")
	send(t, c, websocket.MessageText, `{"event":"resize","cols":120,"rows":50}`)
	send(t, c, websocket.MessageBinary, "\n")
	recv(t, c, "50 120")
	c.Close(websocket.StatusNormalClosure, "")
	f.assertGone(pid)
}

// Test 7 (default size): no cols and rows is 80x24.
func TestConsoleDefaultSize(t *testing.T) {
	f := newConsoleFix(t, "stty size; read x")
	c := f.mustDial(vmConsole)
	recv(t, c, "24 80")
	pid := f.readPid("pid")
	c.Close(websocket.StatusNormalClosure, "")
	f.assertGone(pid)
}

// Test 8: the shell's output is delivered, then close 4000 "exit N".
func TestConsoleExitCode(t *testing.T) {
	for _, code := range []string{"3", "0"} {
		t.Run("exit"+code, func(t *testing.T) {
			f := newConsoleFix(t, "echo bye\nexit "+code)
			c := f.mustDial(vmConsole)
			status, reason, out := closeOf(t, c)
			if !strings.Contains(out, "bye") {
				t.Fatalf("output %q lost before the close", out)
			}
			if status != 4000 || reason != "exit "+code {
				t.Fatalf("close %d %q, want 4000 %q", status, reason, "exit "+code)
			}
			f.assertGone(f.readPid("pid"))
		})
	}
}

// Test 8 (drain): output written right before exit is all delivered, even
// when it is larger than one chunk.
func TestConsoleDrainsOutputBeforeExit(t *testing.T) {
	f := newConsoleFix(t, "i=0\nwhile [ $i -lt 3000 ]; do echo PLACEHOLDER-line-$i-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx; i=$((i+1)); done\necho END-MARK\nexit 5")
	c := f.mustDial(vmConsole)
	status, reason, out := closeOf(t, c)
	if status != 4000 || reason != "exit 5" {
		t.Fatalf("close %d %q", status, reason)
	}
	if !strings.Contains(out, "PLACEHOLDER-line-2999-") || !strings.HasSuffix(strings.TrimSpace(out), "END-MARK") {
		t.Fatalf("output cut short: %d bytes, tail %q", len(out), out[max(0, len(out)-60):])
	}
	f.assertGone(f.readPid("pid"))
}

// Test 13: an inbound message over 64 KiB closes with 1009 and kills the child.
func TestConsoleTooBig(t *testing.T) {
	f := newConsoleFix(t, holdBody)
	c := f.mustDial(vmConsole)
	pid, pid2 := f.readPid("pid"), f.readPid("pid2")
	c.SetReadLimit(1 << 20)
	send(t, c, websocket.MessageBinary, strings.Repeat("x", 70<<10))
	status, reason, _ := closeOf(t, c)
	if status != websocket.StatusMessageTooBig || reason != "message too big" {
		t.Fatalf("close %d %q, want 1009 \"message too big\"", status, reason)
	}
	f.assertGone(pid, pid2)
}

// A message of exactly 64 KiB is accepted.
func TestConsoleMaxMessageAccepted(t *testing.T) {
	f := newConsoleFix(t, "stty -echo\nwhile read l; do echo \"len:${#l}\"; done")
	c := f.mustDial(vmConsole)
	pid := f.readPid("pid")
	// The line discipline's canonical buffer is 4 KiB, so send short lines in
	// one 64 KiB message: the point is the size limit, not the content.
	send(t, c, websocket.MessageBinary, strings.Repeat("a\n", 32<<10))
	recv(t, c, "len:1")
	c.Close(websocket.StatusNormalClosure, "")
	f.assertGone(pid)
}
