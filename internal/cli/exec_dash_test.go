package cli

import (
	"slices"
	"strings"
	"testing"
)

// quotedCommand returns the whole remote command from the stub ssh's args: the
// trailing run of single-quoted elements after the destination (ssh options
// and the destination are never quoted).
func quotedCommand(got []string) []string {
	i := len(got)
	for i > 0 && strings.HasPrefix(got[i-1], "'") {
		i--
	}
	return got[i:]
}

// TestExecDash proves the `--` after the target is the separator and is never
// sent to the guest, for exec and metal exec, with and without -t.
func TestExecDash(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix []string
		target string
	}{
		{"exec", []string{"exec"}, "jetfire/vm"},
		{"metal exec", []string{"metal", "exec"}, "nuc1"},
	} {
		run := func(pre []string, rest ...string) []string {
			a := append(append([]string(nil), tc.prefix...), pre...)
			a = append(a, rest...)
			return runExec(t, a...)
		}
		T := tc.target
		for _, c := range []struct {
			name string
			pre  []string // flags/args before the target
			rest []string // after the target
			want []string // quoted command, the end of ssh's args
			tty  bool
			dash bool // '--' allowed in the recorded command
		}{
			{"sep", nil, []string{"--", "echo", "a b"}, []string{"'echo'", "'a b'"}, false, false},
			{"tty sep", []string{"-t"}, []string{"--", "top"}, []string{"'top'"}, true, false},
			{"sh -c", nil, []string{"--", "sh", "-c", "echo hi | wc -c"}, []string{"'sh'", "'-c'", "'echo hi | wc -c'"}, false, false},
			{"double sep", nil, []string{"--", "--", "x"}, []string{"'--'", "'x'"}, false, true},
			{"inner dash", nil, []string{"echo", "--", "x"}, []string{"'echo'", "'--'", "'x'"}, false, true},
			{"sep inner dash", nil, []string{"--", "echo", "--", "x"}, []string{"'echo'", "'--'", "'x'"}, false, true},
			{"ls -t", nil, []string{"ls", "-t"}, []string{"'ls'", "'-t'"}, false, false},
		} {
			got := run(c.pre, append([]string{T}, c.rest...)...)
			if !slices.Equal(quotedCommand(got), c.want) {
				t.Errorf("%s %s: ssh args %q, want command %q", tc.name, c.name, got, c.want)
			}
			if slices.Contains(got, "-tt") != c.tty {
				t.Errorf("%s %s: -tt = %v, want %v: %q", tc.name, c.name, !c.tty, c.tty, got)
			}
			if !c.dash && slices.Contains(got, "'--'") {
				t.Errorf("%s %s: '--' reached ssh: %q", tc.name, c.name, got)
			}
		}
		// A -- before the target: pflag consumes it, the command runs.
		got := run([]string{"--"}, T, "echo", "x")
		if !slices.Equal(quotedCommand(got), []string{"'echo'", "'x'"}) || slices.Contains(got, "'--'") {
			t.Errorf("%s -- T echo x: ssh args %q", tc.name, got)
		}
		got = run([]string{"--"}, T, "--", "echo", "x")
		if !slices.Equal(quotedCommand(got), []string{"'echo'", "'x'"}) || slices.Contains(got, "'--'") {
			t.Errorf("%s -- T -- echo x: ssh args %q", tc.name, got)
		}
	}
}

// TestExecNoCommand: a target with no command, bare or after the separator,
// is a usage error before any ssh call.
func TestExecNoCommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix []string
		target string
	}{
		{"exec", []string{"exec"}, "jetfire/vm"},
		{"metal exec", []string{"metal", "exec"}, "nuc1"},
	} {
		for _, rest := range [][]string{
			{tc.target},
			{tc.target, "--"},
			{"-t", tc.target, "--"},
			{"-t", tc.target},
		} {
			a := append(append([]string(nil), tc.prefix...), rest...)
			_, err := runExecErr(t, a...)
			if err == nil || !strings.Contains(err.Error(), "no command given after the target") {
				t.Errorf("%s %q: err = %v, want a missing-command error", tc.name, rest, err)
			}
			if _, ok := err.(remoteExitError); ok {
				t.Errorf("%s %q: got remoteExitError, want a plain error", tc.name, rest)
			}
		}
	}
}
