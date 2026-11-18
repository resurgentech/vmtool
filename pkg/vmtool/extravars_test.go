package vmtool

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/testexec"
)

const (
	evK1 = "PLACEHOLDER-not-a-secret-1"
	evK2 = "line1\n{{ not_templated }} $x 'q' \"dq\""
)

func lookupArg(name string) string {
	return name + `={{ lookup("ansible.builtin.env", "VMTOOL_VAR_` + name + `") }}`
}

func TestPlaybookCmdKeepsValuesOutOfArgv(t *testing.T) {
	t.Setenv("VMTOOL_VAR_stale", "old")
	vars := map[string]string{"deviceid": "web", "k1": evK1, "k2": evK2}
	c := playbookCmd(testTunnel(), "/tmp/inv.yml", "pb.yml", vars)

	for _, a := range c.Args {
		for _, bad := range []string{evK1, "not_templated", "line1"} {
			if strings.Contains(a, bad) {
				t.Fatalf("argv element %q holds %q", a, bad)
			}
		}
	}
	want := []string{"ansible-playbook", "--inventory", "/tmp/inv.yml", "pb.yml"}
	for _, k := range []string{"deviceid", "k1", "k2"} {
		want = append(want, "--extra-vars", lookupArg(k))
	}
	if !slices.Equal(c.Args, want) {
		t.Fatalf("argv\n got %q\nwant %q", c.Args, want)
	}
	for _, e := range []string{"VMTOOL_VAR_deviceid=web", "VMTOOL_VAR_k1=" + evK1, "VMTOOL_VAR_k2=" + evK2} {
		if !slices.Contains(c.Env, e) {
			t.Errorf("env lacks %q", e)
		}
	}
	if slices.Contains(c.Env, "VMTOOL_VAR_stale=old") {
		t.Errorf("inherited VMTOOL_VAR_stale survived")
	}
	for _, e := range c.Env {
		if strings.HasPrefix(e, "VMTOOL_VAR_stale") {
			t.Errorf("inherited entry %q survived", e)
		}
	}
	assertTokenOnlyInEnv(t, c.Args, c.Env)
}

func TestCheckExtraVars(t *testing.T) {
	for _, bad := range []map[string]string{
		{"": "x"}, {"1a": "x"}, {"foo-bar": "x"}, {"a.b": "x"}, {"é": "x"},
		{"x": "PLACEHOLDER-not-a-secret-1\x00tail"},
	} {
		err := CheckExtraVars(bad)
		if err == nil {
			t.Errorf("%q accepted", bad)
			continue
		}
		if strings.Contains(err.Error(), "PLACEHOLDER") || strings.Contains(err.Error(), "tail") {
			t.Errorf("error holds the value: %v", err)
		}
	}
	err := CheckExtraVars(map[string]string{"foo-bar": "x"})
	if want := `invalid extra-var name "foo-bar": use letters, digits and _, not starting with a digit`; err == nil || err.Error() != want {
		t.Errorf("got %v, want %s", err, want)
	}
	err = CheckExtraVars(map[string]string{"x": "a\x00b"})
	if want := `extra-var "x": value contains a NUL byte`; err == nil || err.Error() != want {
		t.Errorf("got %v, want %s", err, want)
	}
	if err := CheckExtraVars(map[string]string{"_x": "1", "deviceid": "2", "A1_b": "3"}); err != nil {
		t.Errorf("good names refused: %v", err)
	}
	if err := CheckExtraVars(nil); err != nil {
		t.Errorf("nil refused: %v", err)
	}
}

func TestRunPlaybookValuesViaEnvironment(t *testing.T) {
	d := t.TempDir()
	argvF, envF := filepath.Join(d, "argv"), filepath.Join(d, "env")
	testexec.Stub(t, map[string]string{"ansible-playbook": "#!/bin/sh\n" +
		"echo \"$*\" > '" + argvF + "'\n" +
		"printf %s \"$VMTOOL_VAR_k1\" > '" + envF + "'\n"})
	if _, err := RunPlaybook(Tunnel{}, "/nonexistent/inv.yml", "pb.yml", map[string]string{"k1": evK1}); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(argvF)
	env, _ := os.ReadFile(envF)
	if strings.Contains(string(argv), evK1) {
		t.Errorf("value in argv: %s", argv)
	}
	if string(env) != evK1 {
		t.Errorf("env value = %q, want %q", env, evK1)
	}

	// A bad name: refused, nothing started.
	_ = os.Remove(argvF)
	_ = os.Remove(envF)
	_, err := RunPlaybook(Tunnel{}, "/nonexistent/inv.yml", "pb.yml", map[string]string{"foo-bar": evK1})
	if err == nil || strings.Contains(err.Error(), evK1) {
		t.Fatalf("bad name: err = %v", err)
	}
	for _, f := range []string{argvF, envF} {
		if _, statErr := os.Stat(f); statErr == nil {
			t.Errorf("stub ran: %s exists", f)
		}
	}
}
