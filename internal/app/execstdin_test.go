package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resurgentech/vmtool/internal/testexec"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

const stdinText = "KEY=PLACEHOLDER-not-a-secret-1\nit's=PLACEHOLDER-not-a-secret-2\n"

// recordStubs installs a stub ssh that writes its argv (one per line) to one
// file and its stdin to another, and a stub sshpass that execs it. It returns
// the two file paths.
func recordStubs(t *testing.T) (argvFile, stdinFile string) {
	t.Helper()
	d := t.TempDir()
	argvFile, stdinFile = filepath.Join(d, "argv"), filepath.Join(d, "stdin")
	testexec.Stub(t, map[string]string{
		"ssh":     "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argvFile + "'\ncat > '" + stdinFile + "'\n",
		"sshpass": "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n",
	})
	return argvFile, stdinFile
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExecVMWritesStdin(t *testing.T) {
	t.Chdir(t.TempDir())
	argvFile, stdinFile := recordStubs(t)
	s := (&Service{}).WithTunnel(appTunnel())
	m := &createVMs{ip: "10.0.0.5"}
	res, err := s.ExecVM(context.Background(), m, VMRef{Host: "jetfire", Name: "web"}, []string{"cat"}, stdinText)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if got := readFile(t, stdinFile); got != stdinText {
		t.Fatalf("stdin %q, want %q", got, stdinText)
	}
	if argv := readFile(t, argvFile); strings.Contains(argv, "PLACEHOLDER") {
		t.Fatalf("stdin text in argv:\n%s", argv)
	}
	if _, err := s.ExecVM(context.Background(), m, VMRef{Host: "jetfire", Name: "web"}, []string{"cat"}, ""); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, stdinFile); got != "" {
		t.Fatalf("empty stdin gave %q", got)
	}
}

func TestExecMachineWritesStdin(t *testing.T) {
	for _, auth := range []string{"key", "password"} {
		t.Run(auth, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Kind: vmtool.MachineMetal, Host: "10.0.0.42"}); err != nil {
				t.Fatal(err)
			}
			dir := vmtool.MachineDir("nuc1")
			if auth == "key" {
				if err := os.WriteFile(filepath.Join(dir, "id_ed25519"), []byte("PLACEHOLDER-key-file\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(dir, "password.yml"), []byte("username: bot\npassword: PLACEHOLDER-not-a-secret-3\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			argvFile, stdinFile := recordStubs(t)
			s := &Service{}
			res, err := s.ExecMachine(context.Background(), "nuc1", []string{"cat"}, stdinText)
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("res %+v err %v", res, err)
			}
			if got := readFile(t, stdinFile); got != stdinText {
				t.Fatalf("stdin %q, want %q", got, stdinText)
			}
			if argv := readFile(t, argvFile); strings.Contains(argv, "PLACEHOLDER-not-a-secret-1") || strings.Contains(argv, "PLACEHOLDER-not-a-secret-2") {
				t.Fatalf("stdin text in argv:\n%s", argv)
			}
			if _, err := s.ExecMachine(context.Background(), "nuc1", []string{"cat"}, ""); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, stdinFile); got != "" {
				t.Fatalf("empty stdin gave %q", got)
			}
		})
	}
}

// A command that never reads its stdin still returns when the stdin is bigger
// than a pipe buffer: the EPIPE on the copy is dropped.
func TestExecStdinUnreadLargeReturns(t *testing.T) {
	t.Chdir(t.TempDir())
	testexec.Stub(t, map[string]string{"ssh": "#!/bin/sh\nexit 0\n", "sshpass": "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n"})
	s := (&Service{}).WithTunnel(appTunnel())
	big := strings.Repeat("x", 300*1024)
	type out struct {
		res *ExecResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		res, err := s.ExecVM(context.Background(), &createVMs{ip: "10.0.0.5"}, VMRef{Host: "jetfire", Name: "web"}, []string{"true"}, big)
		ch <- out{res, err}
	}()
	select {
	case o := <-ch:
		if o.err != nil || o.res.ExitCode != 0 {
			t.Fatalf("res %+v err %v", o.res, o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ExecVM did not return with unread large stdin")
	}
}
