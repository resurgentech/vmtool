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

It is a record, not a live view. The store never decides whether a host is up:
a sleeping host keeps its VMs listed as its last report left them. Use
'vmtool list' for the live view.

A daemon sends its full VM list when it starts and after each change made
through vmtool; a new VM's address follows in one more report once it appears
(up to ip_wait_seconds). A change made outside vmtool is fixed at the
next report, or when a vmtool command touches the VM and its host says it does not
have it. The store orders a host's reports by its own clock, so a host with a
wrong clock is not silenced.

A VM its host no longer has is hidden unless --gone is given; it then shows
when the store removes its row and its login. The login is kept 7 days after
that; an empty report never deletes one earlier. 'vmtool delete' removes the
login and the row at once. A VM's origin is known only for VMs vmtool created
from this version on. A comment is kept in plain text, in the VM's definition
and in the store. A VM marked * had its disks unreadable on the host; the
last known disks are shown.

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
