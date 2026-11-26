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
		Short: "Print an ~/.ssh/config block that reaches VMs as <vm>.<host>.vmtool",
		Long: `Print one 'Host *.vmtool' block for ~/.ssh/config. With it ssh, scp, rsync
and sftp reach any VM as <vm>.<host>.vmtool, through 'vmtool tunnel', using
the bot key kept in .machines/<vm>.<host>.vmtool/:

  vmtool ssh-config >> ~/.ssh/config
  ssh web.hv1.vmtool
  scp f vt-r7.hv2.vmtool:/tmp/x

The block is fixed: nothing to regenerate when VMs come and go. Remove an old
'Host */*' block first; it has no credentials folder behind it any more. The
host name has no dots; the VM name may (a.b.hv1.vmtool is VM a.b on
hv1). vmtool's own commands still take host/vm.

The ProxyCommand runs in vmtool's working directory (where config.yml is)
and needs VMTOOL_TOKEN in the environment (or in .env there).`,
		Example: `  vmtool ssh-config --json

  Tip: with --json the block is the "config" string of the object.`,
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
			if jsonMode(cmd) {
				return emit(cmd, map[string]string{"config": block})
			}
			_, err = io.WriteString(cmd.OutOrStdout(), block)
			return err
		},
	}
}

// sshConfigBlock is the 'Host *.vmtool' block for the absolute working
// directory dir. It uses %n, the name as typed, not %h, which ssh lowercases
// (a VM with capitals would miss its folder). ssh expands % tokens in both
// ProxyCommand and IdentityFile, so a % in dir is written %%. ssh runs ProxyCommand as `$SHELL -c "exec <line>"`,
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
	script := "cd " + shellQuote(pct) + " && exec ./vmtool tunnel %n"
	ident := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(pct)
	return fmt.Sprintf(`Host *.vmtool
    ProxyCommand sh -c %s
    User bot
    IdentityFile "%s/.machines/%%n/id_ed25519"
    IdentitiesOnly yes
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
`, shellQuote(script), ident), nil
}

// shellQuote single-quotes s for a POSIX shell, writing ' as '\”.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
