package cli

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/config"
	"github.com/spf13/cobra"
)

// The command-line convention (docs/CLI.md): every non-interactive command
// takes --json and prints exactly one JSON object on stdout; its help has
// usage and an example; an interactive command refuses --json with exit 2.
// TestCLIContract walks the real tree, so a new command is checked without
// being listed anywhere.

// cobraOwn are the commands cobra adds itself (help and shell completion):
// they are cobra's, not vmtool's, and are outside the convention.
func cobraOwn(path string) bool {
	return path == "vmtool help" || path == "vmtool completion" || strings.HasPrefix(path, "vmtool completion ")
}

// jsonCase runs one command with --json against fakes: setup returns the
// working directory and the arguments (without --json); exit is the exit
// code the run must end with (0 unless set).
type jsonCase struct {
	setup func(t *testing.T) (dir string, args []string)
	exit  int
}

// jsonCases has one case per converted command. Each runs the command's real
// RunE against fakes (no libvirt, no host, no store but an in-process one).
var jsonCases = map[string]jsonCase{
	"vmtool key request": {setup: func(t *testing.T) (string, []string) {
		r := newKeyRig(t, "")
		r.putKey(t, "contract-key", 0o600)
		return r.dir, []string{"key", "request"}
	}},
	"vmtool key status": {setup: func(t *testing.T) (string, []string) {
		r := newKeyRig(t, "")
		return r.dir, []string{"key", "status"}
	}},
	"vmtool key check": {setup: func(t *testing.T) (string, []string) {
		r := newKeyRig(t, "")
		local, pool := fakeTLS(t, "contract-key", func(string) (int, string) { return 200, "{}" })
		certPath := filepath.Join(t.TempDir(), "local.pem")
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: local.Certificate().Raw})
		if err := os.WriteFile(certPath, pemBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		lu, _ := url.Parse(local.URL)
		r.dir = writeWorkdir(t, "listen: "+lu.Host+"\ntls_cert: "+certPath+"\n", "")
		old := keyProbeRoots
		keyProbeRoots = pool
		t.Cleanup(func() { keyProbeRoots = old })
		r.putKey(t, "contract-key", 0o600)
		return r.dir, []string{"key", "check"}
	}},
	"vmtool inventory": {setup: func(t *testing.T) (string, []string) {
		s := newCLIStore(t)
		s.inv = inventoryBody()
		return storeWorkdir(t, s, storeTok), []string{"inventory"}
	}},
}

// exampleLine is an example in help: an indented line that runs vmtool. The
// usage block is cut off before it is looked for.
var exampleLine = regexp.MustCompile(`(?m)^ {2,}(\./)?vmtool [a-z]`)

// helpHasExample reports whether help text, past its Usage block, has an
// example line.
func helpHasExample(help string) bool {
	if i := strings.Index(help, "Usage:\n"); i >= 0 {
		rest := help[i:]
		if j := strings.Index(rest, "\n\n"); j >= 0 {
			help = help[:i] + rest[j:]
		} else {
			help = help[:i]
		}
	}
	return exampleLine.MatchString(help)
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// was written there: a command must write through cmd.OutOrStdout(), so in
// these tests anything on the real stdout is a leak.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		defer func() { os.Stdout = old }()
		fn()
	}()
	w.Close()
	s := <-done
	r.Close()
	return s
}

type contractRun struct {
	code   int
	err    error
	out    string // the command's stdout writer
	errOut string // the command's stderr writer
	leak   string // anything written to the process's real stdout
	ran    []string
}

// runContract runs args through run (what Execute does) against the real
// root for dir. Every runnable command except keep gets a tripwire RunE.
func runContract(t *testing.T, dir string, keep string, args ...string) contractRun {
	t.Helper()
	var out, errb bytes.Buffer
	root, e := rootFor(dir, &out, &errb)
	e.tunnelExe = stubTunnelExe
	var r contractRun
	walk(root, func(c *cobra.Command) {
		if !c.Runnable() || c.CommandPath() == keep {
			return
		}
		c.Run = nil
		c.RunE = func(c *cobra.Command, _ []string) error {
			r.ran = append(r.ran, c.CommandPath())
			return nil
		}
	})
	root.SetIn(strings.NewReader(""))
	r.leak = captureStdout(t, func() {
		r.code, r.err = run(root, args, &errb)
	})
	r.out, r.errOut = out.String(), errb.String()
	return r
}

// oneObject reports whether s is exactly one JSON object and nothing else.
func oneObject(s string) error {
	dec := json.NewDecoder(strings.NewReader(s))
	var v any
	if err := dec.Decode(&v); err != nil {
		return err
	}
	if _, ok := v.(map[string]any); !ok {
		return errors.New("not an object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("more than one value")
	}
	return nil
}

// conforms checks one command against the convention and returns what is
// wrong with it (nil when it conforms).
func conforms(t *testing.T, c *cobra.Command, args []string) []string {
	t.Helper()
	path := c.CommandPath()
	var bad []string

	// Help: stdout, exit 0, usage and an example. No token needed.
	t.Setenv(config.TokenEnv, "")
	// The command's words only, then --help: after exec's target the rest of
	// the line is the remote command's, so a dummy argument would hide --help.
	helpArgs := append(strings.Fields(path)[1:], "--help")
	h := runContract(t, writeWorkdir(t, "", ""), "", helpArgs...)
	if h.code != 0 || h.err != nil || !strings.Contains(h.out, "Usage:") || h.errOut != "" {
		bad = append(bad, "help: exit "+itoa(h.code)+", stderr "+h.errOut)
	} else if !helpHasExample(h.out) {
		bad = append(bad, "help has no example")
	}

	if c.Annotations[annotationInteractive] == "true" {
		t.Setenv(config.TokenEnv, gateToken)
		dir := writeWorkdir(t, goodConfig(), "")
		r := runContract(t, dir, path, append([]string{"--json"}, args...)...)
		if r.code != 2 || len(r.ran) != 0 || r.out != "" || r.leak != "" ||
			!strings.Contains(r.errOut, `"interactive command; no --json"`) {
			bad = append(bad, "interactive: exit "+itoa(r.code)+", stderr "+r.errOut)
		}
		return bad
	}

	tc, ok := jsonCases[path]
	if !ok {
		return append(bad, "no --json case")
	}
	dir, cargs := tc.setup(t)
	r := runContract(t, dir, path, append([]string{"--json"}, cargs...)...)
	if r.code != tc.exit {
		bad = append(bad, "--json: exit "+itoa(r.code)+", want "+itoa(tc.exit)+": "+r.errOut)
	}
	if err := oneObject(r.out); err != nil {
		bad = append(bad, "--json stdout: "+err.Error()+": "+r.out)
	}
	if r.leak != "" {
		bad = append(bad, "--json wrote to the real stdout: "+r.leak)
	}
	return bad
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestCLIContract(t *testing.T) {
	cmds := commandArgs(t)
	root, _ := rootFor(t.TempDir(), io.Discard, io.Discard)
	if root.PersistentFlags().Lookup("json") == nil {
		t.Fatal("--json is not a root persistent flag")
	}
	byPath := map[string]*cobra.Command{}
	walk(root, func(c *cobra.Command) {
		byPath[c.CommandPath()] = c
		if c != root && c.LocalNonPersistentFlags().Lookup("json") != nil {
			t.Errorf("%s defines its own --json", c.CommandPath())
		}
	})
	for p := range jsonCases {
		if _, ok := cmds[p]; !ok {
			t.Errorf("jsonCases names %q, which is not a runnable command", p)
		}
	}
	paths := make([]string, 0, len(cmds))
	for p := range cmds {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	checked := 0
	for _, p := range paths {
		if cobraOwn(p) {
			continue
		}
		if bad := conforms(t, byPath[p], cmds[p]); len(bad) > 0 {
			t.Errorf("%s does not conform:\n  %s", p, strings.Join(bad, "\n  "))
		}
		checked++
	}
	t.Logf("%d commands conform (every command but cobra's own)", checked)
}

// Under --json the token gate's refusal is the error object on stderr, with
// code unauthorized and exit 1; nothing on stdout.
func TestJSONGateRefusal(t *testing.T) {
	t.Setenv(config.TokenEnv, "")
	dir := writeWorkdir(t, "", "")
	r := runContract(t, dir, "vmtool inventory", "inventory", "--json")
	if r.code != 1 || r.out != "" || r.leak != "" || len(r.ran) != 0 {
		t.Fatalf("exit %d, out %q, ran %v", r.code, r.out, r.ran)
	}
	var obj struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.errOut), &obj); err != nil || oneObject(r.errOut) != nil {
		t.Fatalf("stderr is not one error object: %v\n%s", err, r.errOut)
	}
	if obj.Error.Code != "unauthorized" || !strings.Contains(obj.Error.Message, "no VMTOOL_TOKEN") {
		t.Errorf("error object %+v", obj.Error)
	}
}

// Without --json the gate's refusal is unchanged: run hands the error back
// for main to print, exit 1, and prints nothing itself.
func TestHumanGateRefusalUnchanged(t *testing.T) {
	t.Setenv(config.TokenEnv, "")
	dir := writeWorkdir(t, "", "")
	r := runContract(t, dir, "vmtool inventory", "inventory")
	if r.code != 1 || r.err == nil || !isGateErr(r.err) || r.errOut != "" || r.out != "" {
		t.Fatalf("exit %d, err %v, stderr %q", r.code, r.err, r.errOut)
	}
}

// A usage error exits 2: in JSON mode as the error object (bad_request), in
// human mode with cobra's usual text and the same line main printed before.
func TestUsageExitCode(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, goodConfig(), "")
	for _, args := range [][]string{
		{"inventory", "--no-such-flag"},
		{"inventory", "extra-arg"},
		{"no-such-command"},
	} {
		r := runContract(t, dir, "vmtool inventory", args...)
		if r.code != 2 || r.err != nil || len(r.ran) != 0 || !strings.Contains(r.errOut, "Error: ") {
			t.Errorf("%v: exit %d, err %v, stderr %q", args, r.code, r.err, r.errOut)
		}
		r = runContract(t, dir, "vmtool inventory", append(args, "--json")...)
		var obj struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if r.code != 2 || r.out != "" || json.Unmarshal([]byte(r.errOut), &obj) != nil || oneObject(r.errOut) != nil || obj.Error.Code != "bad_request" {
			t.Errorf("%v --json: exit %d, out %q, stderr %q", args, r.code, r.out, r.errOut)
		}
	}
}

// A command's own failure in JSON mode is the error object with the code of
// its kind: inventory with no store is store_unavailable.
func TestJSONCommandError(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, "name: ws\ntoken_sha256: "+config.Hash(gateToken)+"\n", "")
	r := runContract(t, dir, "vmtool inventory", "--json", "inventory")
	var obj struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if r.code != 1 || r.out != "" || json.Unmarshal([]byte(r.errOut), &obj) != nil || oneObject(r.errOut) != nil {
		t.Fatalf("exit %d, out %q, stderr %q", r.code, r.out, r.errOut)
	}
	if obj.Error.Code != "store_unavailable" || !strings.Contains(obj.Error.Message, "no store is configured") {
		t.Errorf("error object %+v", obj.Error)
	}
}

// Help goes to stdout with exit 0, and the root help carries the agent tips.
func TestRootHelpAgentTips(t *testing.T) {
	t.Setenv(config.TokenEnv, "")
	r := runContract(t, writeWorkdir(t, "", ""), "", "--help")
	if r.code != 0 || r.errOut != "" {
		t.Fatalf("exit %d, stderr %q", r.code, r.errOut)
	}
	for _, want := range []string{"Agent tips:", "--json", "create", "VMTOOL_TOKEN", ".env", "interactive", "token file", "exits 75", "vmtool key status", "vmtool key check", "VMTOOL_SERVER_TOKEN"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("root help lacks %q", want)
		}
	}
	if !helpHasExample(r.out) {
		t.Error("root help has no example")
	}
}
