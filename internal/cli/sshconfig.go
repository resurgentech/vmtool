package cli

import (
	"fmt"
	"path/filepath"

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
			_, err = fmt.Fprintf(cmd.OutOrStdout(), `Host */*
    ProxyCommand sh -c 'cd %[1]s && exec ./vmtool tunnel %%h'
    User bot
    IdentityFile %[1]s/.machines/%%h/id_ed25519
    IdentitiesOnly yes
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
`, dir)
			return err
		},
	}
}
