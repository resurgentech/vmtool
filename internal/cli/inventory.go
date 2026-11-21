package cli

import (
	"encoding/json"

	"github.com/jaredeh/vmtool/internal/app"

	"github.com/spf13/cobra"
)

func inventoryCmd(e *env) *cobra.Command {
	var (
		host string
		gone bool
		asJS bool
	)
	cmd := &cobra.Command{
		Use:   "inventory",
		Short: "Show the store's inventory of hosts and VMs",
		Long: `Show what the store last heard from each host's daemon: the hosts (when each
last reported, its daemon version, how many VMs it lists live and gone), the
VMs (state, vCPUs, memory, address, disks, what each was made from, when it was
created, its comment, when the store last saw it) and the metal and alien
machines the store keeps.

It is a record, not a live view: it says nothing about whether a host is up,
and a host that is off keeps its VMs listed as its last report left them. Use
'vmtool list' for the live view. A VM its host no longer has is hidden unless
--gone is given; it then shows when the store removes its row and its login.

Needs the store (a server: block in config.yml and VMTOOL_SERVER_TOKEN); with
none, or one that does not answer, it says so and exits 1.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Flags parsed: a failure from here is the store's, not a usage error.
			cmd.SilenceUsage = true
			inv, err := e.service("").Inventory(cmd.Context(), host, gone)
			if err != nil {
				return err
			}
			if asJS {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(inv)
			}
			return app.WriteInventory(cmd.OutOrStdout(), inv, gone)
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "only this host's row and VMs (machines are always listed)")
	cmd.Flags().BoolVar(&gone, "gone", false, "also list VMs their host no longer has, with when they are removed")
	cmd.Flags().BoolVar(&asJS, "json", false, "print the inventory as JSON")
	return cmd
}
