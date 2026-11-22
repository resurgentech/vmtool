package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/config"
	"github.com/spf13/cobra"
)

// Test tokens only; never a real token.
const (
	gateToken = "test-token-5d1c8e"
	gateWrong = "wrong-token-0a9b77"
)

// exemptPaths is the only hand list: commands that run without a valid token.
// Everything else in the tree is found by walking it.
var exemptPaths = map[string]bool{
	"vmtool help":                  true,
	"vmtool completion bash":       true,
	"vmtool completion fish":       true,
	"vmtool completion powershell": true,
	"vmtool completion zsh":        true,
	"vmtool config write":          true,
}

// configOnlyPaths need a valid config.yml with token_sha256 but no local
// token: `vmtool server` on a hypervisor has no .env (deploy writes only the
// hash) and authenticates every request against the hash itself.
var configOnlyPaths = map[string]bool{
	"vmtool server": true,
}

type gateRun struct {
	env    *env
	out    string
	errOut string
	err    error
	ran    []string // command paths whose (stubbed) RunE ran
}

// writeWorkdir makes a temp working dir. cfg is config.yml's body and dotenv
// is .env's; empty means the file is absent.
func writeWorkdir(t *testing.T, cfg, dotenv string) string {
	t.Helper()
	dir := t.TempDir()
	if cfg != "" {
		if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if dotenv != "" {
		if err := os.WriteFile(filepath.Join(dir, config.EnvFileName), []byte(dotenv), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func goodConfig() string {
	return "token_sha256: " + config.Hash(gateToken) + "\nhosts:\n  veritech01: { address: v.labtwo, ssh_user: ubuntu }\n"
}

// rootFor builds the real root command for dir with cobra's help and
// completion commands added, as Execute would. Output is set first because
// cobra's completion command captures its writer when it is added.
func rootFor(dir string, out, errw io.Writer) (*cobra.Command, *env) {
	e := &env{dir: dir}
	root := newRootCmd(e)
	root.SetOut(out)
	root.SetErr(errw)
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	return root, e
}

func walk(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walk(sub, fn)
	}
}

// runGate executes args against the real root for dir. Every runnable command
// except the paths in keep gets a tripwire RunE instead of its own, so no
// libvirt, SSH, TUI or listener can run whatever the gate does.
func runGate(t *testing.T, dir string, keep map[string]bool, args ...string) gateRun {
	t.Helper()
	var out, errb bytes.Buffer
	root, e := rootFor(dir, &out, &errb)
	var r gateRun
	r.env = e
	walk(root, func(c *cobra.Command) {
		if !c.Runnable() || keep[c.CommandPath()] {
			return
		}
		c.Run = nil
		c.RunE = func(c *cobra.Command, _ []string) error {
			r.ran = append(r.ran, c.CommandPath())
			return nil
		}
	})
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)
	r.err = root.Execute()
	r.out, r.errOut = out.String(), errb.String()
	return r
}

// commandArgs returns, for every runnable command, its path words plus enough
// dummy positional args to pass its Args validator (cobra checks Args before
// PersistentPreRunE, so too few args would fail before reaching the gate).
func commandArgs(t *testing.T) map[string][]string {
	t.Helper()
	root, _ := rootFor(t.TempDir(), io.Discard, io.Discard)
	out := map[string][]string{}
	walk(root, func(c *cobra.Command) {
		if !c.Runnable() {
			return
		}
		path := c.CommandPath()
		words := strings.Fields(path)[1:]
		dummy := []string{"x", "x", "x", "x", "x", "x"}
		for n := 0; n <= len(dummy); n++ {
			if c.ValidateArgs(dummy[:n]) == nil {
				out[path] = append(words, dummy[:n]...)
				return
			}
		}
		t.Errorf("%s: no dummy arg count 0..6 satisfies its Args", path)
	})
	if len(out) < 30 {
		t.Fatalf("walked only %d runnable commands: %v", len(out), out)
	}
	for p := range exemptPaths {
		if _, ok := out[p]; !ok {
			t.Errorf("exempt path %q is not in the tree", p)
		}
	}
	return out
}

// TestGateEveryCommand runs every command in the tree under each bad
// token/config state and checks the gate refuses all but the exempt set, then
// that a valid token lets every one through to its RunE.
func TestGateEveryCommand(t *testing.T) {
	cmds := commandArgs(t)
	for _, sc := range []struct {
		name, cfg, dotenv, env string
		want                   error // errors.Is target, nil for a Load error
	}{
		{name: "nothing", want: config.ErrNoToken},
		{name: "no token", cfg: goodConfig(), want: config.ErrNoToken},
		{name: "no config", env: gateToken, want: config.ErrNotFound},
		{name: "mismatch", cfg: goodConfig(), env: gateWrong, want: config.ErrTokenMismatch},
		{name: "mismatch dotenv", cfg: goodConfig(), dotenv: "VMTOOL_TOKEN=" + gateWrong + "\n", want: config.ErrTokenMismatch},
		{name: "empty hash", cfg: "name: a\n", env: gateToken, want: config.ErrTokenMismatch},
		{name: "invalid config", cfg: "token_sha256: nothex\n", env: gateToken},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Setenv(config.TokenEnv, sc.env)
			dir := writeWorkdir(t, sc.cfg, sc.dotenv)
			for path, args := range cmds {
				r := runGate(t, dir, nil, args...)
				if configOnlyPaths[path] {
					// The daemon needs a valid config.yml with a hash, not a
					// local token: every request it serves carries one.
					wantRun := sc.cfg == goodConfig()
					if wantRun && (r.err != nil || len(r.ran) != 1) {
						t.Errorf("%s (config only): err %v, ran %v", path, r.err, r.ran)
					}
					if !wantRun && (len(r.ran) != 0 || !isGateErr(r.err)) {
						t.Errorf("%s (config only): err %v, ran %v, want the gate's error", path, r.err, r.ran)
					}
					continue
				}
				if exemptPaths[path] {
					if isGateErr(r.err) || len(r.ran) != 1 {
						t.Errorf("%s (exempt): err %v, ran %v", path, r.err, r.ran)
					}
					continue
				}
				if len(r.ran) != 0 {
					t.Errorf("%s: RunE ran without a valid token", path)
				}
				if !isGateErr(r.err) {
					t.Errorf("%s: err %v, want the gate's error", path, r.err)
					continue
				}
				if sc.want != nil && !errors.Is(r.err, sc.want) {
					t.Errorf("%s: err %v, want %v", path, r.err, sc.want)
				}
				if strings.Contains(r.out+r.errOut, "Usage:") || r.errOut != "" {
					t.Errorf("%s: cobra printed on a gate failure: out %q err %q", path, r.out, r.errOut)
				}
				for _, tok := range []string{gateToken, gateWrong} {
					if strings.Contains(r.err.Error(), tok) {
						t.Errorf("%s: gate error contains a token", path)
					}
				}
			}
		})
	}

	t.Run("valid", func(t *testing.T) {
		t.Setenv(config.TokenEnv, "")
		dir := writeWorkdir(t, goodConfig(), "VMTOOL_TOKEN="+gateToken+"\n")
		for path, args := range cmds {
			r := runGate(t, dir, nil, args...)
			if r.err != nil || len(r.ran) != 1 || r.ran[0] != path {
				t.Errorf("%s: err %v, ran %v", path, r.err, r.ran)
			}
		}
	})
}

func isGateErr(err error) bool {
	var g *gateError
	return errors.As(err, &g)
}

func TestGateMessages(t *testing.T) {
	t.Setenv(config.TokenEnv, "")
	dir := writeWorkdir(t, "", "")
	r := runGate(t, dir, nil, "list")
	want := "vmtool: no VMTOOL_TOKEN in the environment or " + dir + "/.env; run 'just token' (new) or 'just token-fetch' (from OpenBao)"
	if r.err == nil || r.err.Error() != want {
		t.Errorf("no token:\n got %v\nwant %s", r.err, want)
	}

	t.Setenv(config.TokenEnv, gateToken)
	r = runGate(t, dir, nil, "list")
	want = "vmtool: no config.yml in " + dir + "; run 'just install' (or 'just deploy <host>' on a hypervisor)"
	if r.err == nil || r.err.Error() != want {
		t.Errorf("no config:\n got %v\nwant %s", r.err, want)
	}

	dir = writeWorkdir(t, goodConfig(), "")
	t.Setenv(config.TokenEnv, gateWrong)
	r = runGate(t, dir, nil, "list")
	want = "vmtool: VMTOOL_TOKEN does not match token_sha256 in config.yml; run 'just token-fetch', or 'just install' after 'just token'"
	if r.err == nil || r.err.Error() != want {
		t.Errorf("mismatch:\n got %v\nwant %s", r.err, want)
	}

	dir = writeWorkdir(t, "role: boss\n", "")
	t.Setenv(config.TokenEnv, gateToken)
	r = runGate(t, dir, nil, "list")
	_, lerr := config.Load(dir)
	if lerr == nil || r.err == nil || r.err.Error() != "vmtool: "+lerr.Error() {
		t.Errorf("invalid config: got %v, want vmtool: %v", r.err, lerr)
	}
	if r.err != nil && strings.Contains(r.err.Error(), "\n") {
		t.Errorf("message is not one line: %q", r.err)
	}
}

// Help never needs a token.
func TestGateHelpWithoutToken(t *testing.T) {
	t.Setenv(config.TokenEnv, "")
	dir := writeWorkdir(t, "", "")
	keep := map[string]bool{"vmtool help": true}
	for _, args := range [][]string{
		{}, {"--help"}, {"-h"}, {"help"}, {"help", "create"}, {"help", "metal", "add"},
		{"create", "--help"}, {"metal", "add", "-h"}, {"config", "host", "--help"},
		{"server", "--help"}, {"metal"}, {"images"}, {"config"},
	} {
		r := runGate(t, dir, keep, args...)
		if r.err != nil || !strings.Contains(r.out, "Usage:") || len(r.ran) != 0 {
			t.Errorf("%v: err %v, ran %v, out %q", args, r.err, r.ran, r.out)
		}
	}
}

// Shell completion requests never need a token.
func TestGateCompletionWithoutToken(t *testing.T) {
	t.Setenv(config.TokenEnv, "")
	dir := writeWorkdir(t, "", "")
	for _, req := range []string{cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd} {
		r := runGate(t, dir, nil, req, "")
		if r.err != nil || !strings.Contains(r.out, "create") {
			t.Errorf("%s: err %v, out %q", req, r.err, r.out)
		}
	}
	r := runGate(t, dir, map[string]bool{"vmtool completion bash": true}, "completion", "bash")
	if r.err != nil || !strings.Contains(r.out, "bash completion") {
		t.Errorf("completion bash: err %v, out %.80q", r.err, r.out)
	}
}

// A valid token reaches a real RunE; config host touches no libvirt.
func TestGatePassesConfigHost(t *testing.T) {
	t.Setenv(config.TokenEnv, "")
	dir := writeWorkdir(t, goodConfig(), "VMTOOL_TOKEN="+gateToken+"\n")
	r := runGate(t, dir, map[string]bool{"vmtool config host": true}, "config", "host", "veritech01")
	if r.err != nil || r.out != "v.labtwo ubuntu 9473\n" {
		t.Fatalf("err %v, out %q", r.err, r.out)
	}
	if r.env.cfg == nil || r.env.cfg.Dir != dir || r.env.token != gateToken {
		t.Fatalf("gate did not keep config and token: cfg %+v", r.env.cfg)
	}
}

// VMTOOL_TOKEN in the environment wins over .env.
func TestGateEnvWinsOverDotEnv(t *testing.T) {
	keep := map[string]bool{"vmtool config host": true}
	dir := writeWorkdir(t, goodConfig(), "VMTOOL_TOKEN="+gateWrong+"\n")
	t.Setenv(config.TokenEnv, gateToken)
	if r := runGate(t, dir, keep, "config", "host", "veritech01"); r.err != nil {
		t.Fatalf("good env, bad .env: %v", r.err)
	}
	dir = writeWorkdir(t, goodConfig(), "VMTOOL_TOKEN="+gateToken+"\n")
	t.Setenv(config.TokenEnv, gateWrong)
	if r := runGate(t, dir, keep, "config", "host", "veritech01"); !errors.Is(r.err, config.ErrTokenMismatch) {
		t.Fatalf("bad env, good .env: %v", r.err)
	}
}

// config write bootstraps config.yml without one, but still needs a token.
func TestGateConfigWriteBootstrap(t *testing.T) {
	keep := map[string]bool{"vmtool config write": true}
	dir := writeWorkdir(t, "", "")
	t.Setenv(config.TokenEnv, "")
	if r := runGate(t, dir, keep, "config", "write"); !errors.Is(r.err, config.ErrNoToken) {
		t.Fatalf("no token: %v", r.err)
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); err == nil {
		t.Fatal("config.yml written without a token")
	}
	t.Setenv(config.TokenEnv, gateToken)
	if r := runGate(t, dir, keep, "config", "write"); r.err != nil {
		t.Fatal(r.err)
	}
	c, err := config.Load(dir)
	if err != nil || c.TokenSHA256 != config.Hash(gateToken) {
		t.Fatalf("config %+v, %v", c, err)
	}
}

// vmtool server passes the gate, then refuses to serve without TLS. The
// listen address is TEST-NET-1, so even a missing check could not bind.
func TestGateServerNeedsTLS(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, goodConfig(), "")
	r := runGate(t, dir, map[string]bool{"vmtool server": true}, "server", "--listen", "192.0.2.1:9473")
	if r.err == nil || isGateErr(r.err) || !strings.Contains(strings.ToLower(r.err.Error()), "tls") {
		t.Fatalf("err %v, want a TLS refusal", r.err)
	}
}
