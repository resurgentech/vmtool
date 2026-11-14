package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func sshConfigCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh-config",
		Short: "Print an ~/.ssh/config block that reaches VMs as host/vm",
		Long: `Print a 'Host */*' block for ~/.ssh/config. With it, plain ssh and scp
reach any VM through 'vmtool tunnel', using the bot key kept in
.machines/<host>/<vm>/:

  vmtool ssh-config >> ~/.ssh/config
  ssh jetfire/web

The ProxyCommand runs in vmtool's working directory (where config.yml is)
and needs VMTOOL_TOKEN in the environment (or in .env there).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := filepath.Abs(e.dir)
			if err != nil {
				return err
			}
			block, err := sshConfigBlock(dir)
			if err != nil {
				return err
			}
			_, err = io.WriteString(cmd.OutOrStdout(), block)
			return err
		},
	}
}

// sshConfigBlock is the 'Host */*' block for the absolute working directory
// dir. ssh expands % tokens in both ProxyCommand and IdentityFile, so a %
// in dir is written %%. ssh runs ProxyCommand as `$SHELL -c "exec <line>"`,
// so the line is `sh -c '<script>'`: the directory is shell-quoted inside
// the script and the script is quoted again for the outer shell.
// IdentityFile uses ssh_config double quotes, where \" and \\ are escapes.
// A directory with a control character (newline, tab, NUL, ...) cannot be
// written safely and is an error.
func sshConfigBlock(dir string) (string, error) {
	for _, r := range dir {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("ssh-config: working directory %q contains a control character and cannot be written to ssh_config", dir)
		}
	}
	pct := strings.ReplaceAll(dir, "%", "%%")
	script := "cd " + shellQuote(pct) + " && exec ./vmtool tunnel %h"
	ident := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(pct)
	return fmt.Sprintf(`Host */*
    ProxyCommand sh -c %s
    User bot
    IdentityFile "%s/.machines/%%h/id_ed25519"
    IdentitiesOnly yes
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
`, shellQuote(script), ident), nil
}

// shellQuote single-quotes s for a POSIX shell, writing ' as '\”.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
