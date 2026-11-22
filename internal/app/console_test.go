package app

import (
	"context"
	"testing"

	"github.com/resurgentech/vmtool/internal/testexec"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

func kindIs(err error, want Kind) bool {
	var ae *Error
	return AsError(err, &ae) && ae.Kind == want
}

func consoleStubs(t *testing.T) {
	t.Helper()
	testexec.Stub(t, map[string]string{"ssh": "#!/bin/sh\nexit 0\n", "sshpass": "#!/bin/sh\nexit 0\n"})
}

func TestConsoleVMArgv(t *testing.T) {
	t.Chdir(t.TempDir())
	consoleStubs(t)
	s := (&Service{}).WithTunnel(appTunnel())
	c, err := s.ConsoleVM(context.Background(), &createVMs{ip: "10.0.0.5"}, VMRef{Host: "jetfire", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	// sshpass -e ssh <opts> -tt jetfire/web: nothing follows the destination.
	n := len(c.Args)
	if n < 3 || c.Args[n-1] != "jetfire/web" || c.Args[n-2] != "-tt" {
		t.Fatalf("argv %q: want ... -tt jetfire/web, no remote command", c.Args)
	}
	if c.Stdin != nil || c.Stdout != nil || c.Stderr != nil {
		t.Fatal("console command must leave stdio to the caller")
	}
}

func TestConsoleVMRefusals(t *testing.T) {
	t.Chdir(t.TempDir())
	consoleStubs(t)
	s := (&Service{}).WithTunnel(appTunnel())
	if _, err := s.ConsoleVM(context.Background(), &createVMs{}, VMRef{Host: "jetfire", Name: "web"}); !kindIs(err, KindConflict) {
		t.Fatalf("no IP: %v", err)
	}
	if _, err := s.ConsoleVM(context.Background(), &createVMs{ip: "10.0.0.5"}, VMRef{Host: "jet fire", Name: "web"}); !kindIs(err, KindInvalid) {
		t.Fatalf("bad ref: %v", err)
	}
}

func TestConsoleMachineArgv(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Kind: vmtool.MachineMetal, Host: "10.0.0.42"}); err != nil {
		t.Fatal(err)
	}
	consoleStubs(t)
	s := &Service{}
	c, err := s.ConsoleMachine("nuc1")
	if err != nil {
		t.Fatal(err)
	}
	n := len(c.Args)
	if n < 2 || c.Args[n-2] != "-tt" {
		t.Fatalf("argv %q: want -tt just before the destination, no remote command", c.Args)
	}
	if c.Stdin != nil || c.Stdout != nil || c.Stderr != nil {
		t.Fatal("console command must leave stdio to the caller")
	}
	if _, err := s.ConsoleMachine("nope"); !kindIs(err, KindNotFound) {
		t.Fatalf("unknown machine: %v", err)
	}
	if _, err := s.ConsoleMachine("bad name"); !kindIs(err, KindInvalid) {
		t.Fatalf("bad name: %v", err)
	}
}
