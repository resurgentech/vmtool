package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// errNoExecCommand is the usage error for exec and metal exec when the target
// is not followed by a command.
var errNoExecCommand = errors.New("exec: no command given after the target")

// execCommand returns the command argv from exec's positional args (args[0] is
// the target). A "--" directly after the target is the separator between
// vmtool's flags and the command and is dropped; any other "--" belongs to the
// command.
func execCommand(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	return rest
}

// execArgs validates the positional args of exec and metal exec: a target and
// at least one command element after the optional separator.
func execArgs(cmd *cobra.Command, args []string) error {
	if len(execCommand(args)) == 0 {
		return errNoExecCommand
	}
	return nil
}
