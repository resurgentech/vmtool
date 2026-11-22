package cli

import (
	"fmt"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/spf13/cobra"
)

func credsCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "creds",
		Short: "Fetch or forget a VM's guest credentials cache (store mode)",
		Long: `With a store configured (server: in config.yml) the credentials of every VM
live in the store, and .machines/<vm>.<host>.vmtool/ on this machine is a
cache of that record: plain ssh, scp, rsync and Ansible read the bot key from
it (ssh-config's IdentityFile points there).

  vmtool creds fetch jetfire/web     write the store's record into the cache
  vmtool creds forget jetfire/web    remove the cache folder (not the store's record)

Both decide the store mode at their start. Without a reachable store the cache
is the only copy: fetch has nothing to fetch from, and forget refuses.`,
	}
	cmd.AddCommand(credsFetchCmd(e), credsForgetCmd(e))
	return cmd
}

func credsFetchCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "fetch <host/vm>",
		Short: "Write the store's credentials for a VM into .machines/ (replaces the cache)",
		Long: `Fetch the store's record for the VM's current domain UUID and write it into
.machines/<vm>.<host>.vmtool/ (0700, files 0600), replacing what is there. The
store answers only for the UUID the VM's host reports, so credentials left
over from a VM of the same name that was recreated are never fetched.

Needs store mode: a configured, reachable store.`,
		Args: cobra.ExactArgs(1),
		RunE: withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
			if err := e.service("").FetchVMCreds(cmd.Context(), m, ref); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Credentials for %s fetched into the cache\n", ref)
			return nil
		}),
	}
}

func credsForgetCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "forget <host/vm>",
		Short: "Remove a VM's credentials cache folder from .machines/ (the store's record stays)",
		Long: `Remove .machines/<vm>.<host>.vmtool/ on this machine. The store's record is
not touched: the next command that uses the VM, or 'creds fetch', fetches it
again. 'vmtool delete' is what removes the record.

Refuses without a store (the folder is then the only copy) and when the
folder holds credentials that are not in the store yet.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := e.resolver().ParseVM(args[0])
			if err != nil {
				return err
			}
			if err := e.service("").ForgetVMCreds(cmd.Context(), ref); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Credentials cache for %s removed (the store's record is untouched)\n", ref)
			return nil
		},
	}
}
