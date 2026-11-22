package cli

// The output layer of the command-line convention (docs/CLI.md): --json, the
// error object, and the exit codes. A command checks jsonMode, and in JSON
// mode prints its result with emit and nothing else on stdout; its human text
// and progress go to stderr. Errors are printed once, by run.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
	"github.com/spf13/cobra"
)

const (
	// jsonFlagName is the root persistent flag every command inherits.
	jsonFlagName = "json"
	// annotationInteractive marks a command that is exempt from --json (a
	// TUI, a shell, a tunnel, a server): with --json it exits 2.
	annotationInteractive = "interactive"
	// interactiveRefusal is that exit's message.
	interactiveRefusal = "interactive command; no --json"
)

// Exit codes (docs/CLI.md).
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// jsonMode reports whether cmd runs with --json.
func jsonMode(cmd *cobra.Command) bool {
	f := cmd.Flag(jsonFlagName)
	return f != nil && f.Value.String() == "true"
}

// emit prints v as the command's one JSON object on stdout. v must encode to
// an object: a list goes under a named key, never bare.
func emit(cmd *cobra.Command, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fail(string(api.ErrorCodeInternal), fmt.Errorf("encoding the result: %w", err))
	}
	if len(b) == 0 || b[0] != '{' {
		return fail(string(api.ErrorCodeInternal), errors.New("result is not a JSON object"))
	}
	_, err = cmd.OutOrStdout().Write(append(b, '\n'))
	return err
}

// humanOut is where a command's human text goes: stdout normally, stderr in
// JSON mode (stdout then carries only the object).
func humanOut(cmd *cobra.Command) io.Writer {
	if jsonMode(cmd) {
		return cmd.ErrOrStderr()
	}
	return cmd.OutOrStdout()
}

// cliError is an error with its convention code and exit code.
type cliError struct {
	code string
	exit int
	err  error
}

func (c *cliError) Error() string { return c.err.Error() }
func (c *cliError) Unwrap() error { return c.err }

// fail tags err with code (one of the API's Error.code values), exit 1. The
// error is printed once, by run: as the error object with --json, else as
// the usual line.
func fail(code string, err error) error {
	if err == nil {
		return nil
	}
	return &cliError{code: code, exit: exitError, err: err}
}

// usageError is a usage refusal: code bad_request, exit 2.
func usageError(err error) error {
	return &cliError{code: string(api.ErrorCodeBadRequest), exit: exitUsage, err: err}
}

// runError marks an error a command's RunE returned, as against one cobra
// returned while parsing the command line (a usage error).
type runError struct{ err error }

func (r runError) Error() string { return r.err.Error() }
func (r runError) Unwrap() error { return r.err }

// markRunErrors wraps every RunE in the tree so its errors are told apart
// from cobra's usage errors.
func markRunErrors(c *cobra.Command) {
	if c.RunE != nil {
		orig := c.RunE
		c.RunE = func(cmd *cobra.Command, args []string) error {
			err := orig(cmd, args)
			if _, remote := err.(remoteExitError); err == nil || remote {
				// A remote command's status passes through as it is.
				return err
			}
			return runError{err}
		}
	}
	for _, sub := range c.Commands() {
		markRunErrors(sub)
	}
}

// classify gives err's code and exit code.
func classify(err error) (code string, exit int) {
	var ce *cliError
	var ge *gateError
	var ae *app.Error
	var re runError
	switch {
	case errors.As(err, &ce):
		return ce.code, ce.exit
	case errors.As(err, &ge):
		return string(api.ErrorCodeUnauthorized), exitError
	case !errors.As(err, &re):
		// Not from a command's RunE, the gate or a tagged refusal: cobra
		// refused the command line.
		return string(api.ErrorCodeBadRequest), exitUsage
	case errors.As(err, &ae):
		return codeForKind(ae.Kind), exitError
	case vmtool.IsNotFound(err):
		return string(api.ErrorCodeNotFound), exitError
	}
	return string(api.ErrorCodeInternal), exitError
}

// codeForKind is the code of an app error kind: the same words the REST API
// sends in Error.code.
func codeForKind(k app.Kind) string {
	switch k {
	case app.KindInvalid:
		return string(api.ErrorCodeBadRequest)
	case app.KindNotFound:
		return string(api.ErrorCodeNotFound)
	case app.KindConflict:
		return string(api.ErrorCodeConflict)
	case app.KindBadGateway:
		return string(api.ErrorCodeBadGateway)
	case app.KindUnavailable:
		return string(api.ErrorCodeStoreUnavailable)
	case app.KindHostUnreachable:
		return string(api.ErrorCodeHostUnreachable)
	case app.KindHostTimeout:
		return string(api.ErrorCodeHostTimeout)
	}
	return string(api.ErrorCodeInternal)
}

// wantsJSON reports whether args ask for --json, read before cobra parses
// them (so a refused command line still gets the error object). Arguments
// after "--" belong to another program.
func wantsJSON(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--json" || a == "--json=true" {
			return true
		}
	}
	return false
}

// run executes root with args and returns the exit code. An error is
// printed here, once: with --json as the error object on errw, and nothing
// else; without it a usage error's line is printed here (exit 2) and any
// other error is returned for main to print (exit 1), as before. A remote
// command's exit status (exec) is passed through, printing nothing.
func run(root *cobra.Command, args []string, errw io.Writer) (int, error) {
	pre := wantsJSON(args)
	if pre {
		// cobra prints usage errors itself unless silenced.
		root.SilenceErrors, root.SilenceUsage = true, true
	}
	root.SetArgs(args)
	cmd, err := root.ExecuteC()
	if err == nil {
		return exitOK, nil
	}
	var remote remoteExitError
	if errors.As(err, &remote) {
		return remote.code, nil
	}
	code, exit := classify(err)
	asJSON := pre
	if exit != exitUsage && cmd != nil {
		// Parsed: the flag itself decides (an argument after the target of
		// exec belongs to the remote command, not to vmtool).
		asJSON = jsonMode(cmd)
	}
	if asJSON {
		writeErrorObject(errw, code, err.Error())
		return exit, nil
	}
	if exit == exitError {
		return exit, err
	}
	fmt.Fprintln(errw, err)
	return exit, nil
}

// writeErrorObject prints the error object: one line of JSON.
func writeErrorObject(w io.Writer, code, msg string) {
	var b bytes.Buffer
	_ = json.NewEncoder(&b).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
	_, _ = w.Write(b.Bytes())
}
