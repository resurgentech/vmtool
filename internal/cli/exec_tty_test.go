package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/testexec"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// ipFake is a Manager whose Info answers with an address, so exec proceeds to ssh.
type ipFake struct{ cliFake }

func (ipFake) Info(string) (*vmtool.VMInfo, error) {
	return &vmtool.VMInfo{Name: "vm", IP: "192.0.2.9"}, nil
}

// runExec runs the real exec / metal exec command with a stub ssh that writes
// its arguments, one per line, to a file, and returns those lines.
func runExec(t *testing.T, args ...string) []string {
	t.Helper()
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	t.Chdir(dir)
	if err := vmtool.SaveMachine(vmtool.Machine{Name: "nuc1", Kind: vmtool.MachineMetal, Host: "192.0.2.7"}); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "ssh-args")
	testexec.Stub(t, map[string]string{
		"ssh":     "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + log + "'\n",
		"sshpass": "#!/bin/sh\nshift 2\nexec ssh \"$@\"\n",
	})
	old := newResolver
	t.Cleanup(func() { newResolver = old })
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
			return ipFake{cliFake{host: e.cfg.Name, f: &fakeHosts{}}}, func() {}, nil
		})
	}
	root, e := rootFor(dir, os.Stderr, os.Stderr)
	e.tunnelExe = "/opt/vmtool/vmtool"
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("stub ssh did not run: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func tail(a []string, n int) []string { return a[len(a)-n:] }

func TestExecTTYFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		dest string
	}{
		{"exec", []string{"exec"}, "jetfire/web"},
		{"metal exec", []string{"metal", "exec"}, "nuc1"},
	} {
		with := func(rest ...string) []string {
			return append(append([]string(nil), tc.args...), rest...)
		}
		target := tc.dest
		if tc.name == "exec" {
			target = "jetfire/vm"
		}
		got := runExec(t, with("-t", target, "--", "echo", "a b")...)
		if !slices.Contains(got, "-tt") || got[len(got)-2] != "'echo'" || got[len(got)-1] != "'a b'" {
			t.Errorf("%s -t: ssh args %q, want -tt and quoted argv", tc.name, got)
		}
		for i, a := range got {
			if a == "-tt" && i+1 < len(got) && strings.HasPrefix(got[i+1], "-") && got[i+1] != "-l" {
				t.Errorf("%s -t: -tt not before the destination: %q", tc.name, got)
			}
		}
		got = runExec(t, with(target, "--", "echo", "a b")...)
		if slices.Contains(got, "-tt") || got[len(got)-1] != "'a b'" {
			t.Errorf("%s: plain ssh args %q", tc.name, got)
		}
		got = runExec(t, with(target, "ls", "-t")...)
		if slices.Contains(got, "-tt") || !slices.Equal(tail(got, 2), []string{"'ls'", "'-t'"}) {
			t.Errorf("%s ls -t: ssh args %q, want no -tt and -t passed to ls", tc.name, got)
		}
	}
}
