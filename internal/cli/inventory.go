package cli

import (
	"encoding/json"

	"github.com/resurgentech/vmtool/internal/app"

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
		Short: "Show the store's inventory of hosts, VMs and templates",
		Long: `Show what the store last heard from each host's daemon: the hosts (when each
last reported, its daemon version, how many VMs it lists live and gone, how
many templates it reported), the
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
login and the row at once. Origin and comment are the VM's notes, kept in the
store only (vmtool writes nothing of its own into a VM's libvirt definition),
in plain text; no report touches them. A VM's origin is recorded once, at
create, and only with a store in use, so it is known only for VMs vmtool
created that way from this version on. The comment is set with 'vmtool
comment', also while the host sleeps. A VM marked * had its disks unreadable
on the host; the last known disks are shown.

Templates are listed too: each host's thin-pool templates, image or saved, with
the pool, the image an image template was built from, its size and when it was
made when the host knows that. They are the last list each daemon sent with its
VMs, so a sleeping host's templates stay listed. A host row says how many
templates it reported, or "not reported" when its daemon is too old to send
them (it is not "none"). A template is not hidden or removed when its host stops
reporting. --json includes them.

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
	cmd.Flags().StringVar(&host, "host", "", "only this host's row, VMs and templates (machines are always listed)")
	cmd.Flags().BoolVar(&gone, "gone", false, "also list VMs their host no longer has, with when they are removed")
	cmd.Flags().BoolVar(&asJS, "json", false, "print the inventory as JSON")
	return cmd
}
