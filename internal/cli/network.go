package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// netCmd is `vmtool net`: named networks on one host (V1). They need no
// store; a network is a libvirt network on the host whose bridge is vmt- plus
// six lowercase hex digits.
func netCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "net",
		Short: "Create, show and delete vmtool networks on a host",
		Long: `A vmtool network is a libvirt network vmtool made: NAT, DHCP and DNS on a /24
of 10.77.0.0/16, a bridge called vmt- plus six hex digits, MTU 1450. vmtool
writes nothing of its own into the network's definition; the bridge name is
what marks it as vmtool's. vmtool leaves every other libvirt network alone.

A network can span hosts: other hosts join it (net join) and their VMs then
talk to the network's home and to each other over VXLAN. See net join.

'vmtool networks' writes "name (vmtool)" for a vmtool network and
"name (vmtool, member)" for the member form on a host that is not its home.
Reservations never expire, so a VM keeps its address. If the home is down,
VM-to-VM traffic by address keeps working; names, new addresses and NAT do not.
A network that was started by hand (virsh net-destroy, net-start) leaves the
VMs' cards detached: restart libvirtd, then the vmtool daemon (its event watch
does not survive a libvirtd restart). A network that spans hosts is not removed
from the registry by net delete yet. The README's "VM networks" section has
the rest.`,
	}
	cmd.AddCommand(netCreateCmd(e), netDeleteCmd(e), netShowCmd(e), netAttachCmd(e), netDetachCmd(e), netJoinCmd(e), netLeaveCmd(e))
	return cmd
}

func netAttachCmd(e *env) *cobra.Command {
	var ip string
	cmd := &cobra.Command{
		Use:   "attach <host/vm> <network>",
		Short: "Give a VM a card on a vmtool network, with a fixed address and its name",
		Long: `Attach an extra card on the vmtool network <network> (on the VM's host) to the
VM: vmtool picks the MAC, reserves --ip (or the next free address of the
network's DHCP range) under the VM's name, then adds the card, live and in the
config when the VM is running, in the config only when it is shut off. If the
card cannot be added the reservation is removed again. The VM's own first card
stays first, and 'vmtool info' still shows its address as the VM's IP. The VM
resolves as 'vm' and 'vm.<domain>' on the network. A network vmtool did not
make is refused.

  vmtool net attach hv1/web net1
  vmtool net attach hv1/web net2 --ip 10.77.2.50`,
		Args: cobra.ExactArgs(2),
	}
	cmd.Flags().StringVar(&ip, "ip", "", "the fixed address, inside the network's subnet (default: the next free address of its DHCP range)")
	cmd.RunE = withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
		a, err := e.service("").AttachNetwork(cmd.Context(), m, ref, args[1], ip)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "VM %q attached to network %q\n", ref.String(), a.Network)
		writeCards(cmd.OutOrStdout(), []vmtool.NetAttachment{*a})
		return nil
	})
	return cmd
}

func netDetachCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "detach <host/vm> <network|mac>",
		Short: "Remove a VM's card on a vmtool network and its reservation",
		Long: `Remove the VM's card on the vmtool network <network> (live and from the config
when the VM is running, from the config only when it is shut off), then its
reservation. Name the card by its MAC instead when the VM has more than one on
the network. If the reservation cannot be removed the card is put back. The
VM's own first card is never removed by this command.`,
		Args: cobra.ExactArgs(2),
	}
	cmd.RunE = withVM(e, func(m app.Manager, ref app.VMRef, cmd *cobra.Command, args []string) error {
		if err := e.service("").DetachNetwork(cmd.Context(), m, ref, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "VM %q detached from %q\n", ref.String(), args[1])
		return nil
	})
	return cmd
}

// writeCards prints a VM's cards on vmtool networks: one line each, labelled on
// the first, nothing when there is none.
func writeCards(w io.Writer, cards []vmtool.NetAttachment) {
	for i, c := range cards {
		label := ""
		if i == 0 {
			label = "Networks:"
		}
		ip := c.IP
		if ip == "" {
			ip = "-"
		}
		fmt.Fprintf(w, "%-10s %s %s %s\n", label, c.Network, c.MAC, ip)
	}
}

func netCreateCmd(e *env) *cobra.Command {
	var subnet, domain string
	cmd := &cobra.Command{
		Use:   "create <host> <name>",
		Short: "Make a network on a host",
		Long: `Make a vmtool network called <name> on <host>: NAT, DHCP (.100 to .200), DNS
and autostart. The DNS domain is --domain, or <name>.test. <name> follows the
VM name rule and is never "default".

With a store, the network is registered there first: the registry gives it
its VNI (from 10000, so the bridge is vmt- plus the VNI as six hex digits) and
its subnet (the next free /24 of 10.77.0.0/16, or --subnet if free), and the
host makes the libvirt network with that bridge number and subnet. If the
host refuses (a bridge of that number or an overlapping subnet), the registry
entry is removed again. A host running an older vmtool that cannot take a
network number is refused: redeploy it (just deploy <host>).

With no store (or one that does not answer), nothing is registered: the host
numbers the bridge itself (1 to 9999) and takes --subnet, or the first /24 of
10.77.0.0/16 that no libvirt network or route on the host uses; an overlap is
refused.`,
		Args: cobra.ExactArgs(2),
	}
	cmd.Flags().StringVar(&subnet, "subnet", "", "the network's /24, such as 10.77.1.0/24 (default: the first free /24 of 10.77.0.0/16)")
	cmd.Flags().StringVar(&domain, "domain", "", "DNS domain (default: <name>.test)")
	cmd.RunE = withNetHost(e, func(m app.Manager, cmd *cobra.Command, args []string) error {
		n, err := e.service("").CreateNetworkOnHost(cmd.Context(), args[0], m, args[1], subnet, domain)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Network %q created on %s\n", n.Name, args[0])
		writeNetwork(cmd.OutOrStdout(), n)
		return nil
	})
	return cmd
}

func netDeleteCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <host> <name>",
		Short: "Delete a network vmtool made on a host",
		Long: `Stop and remove the vmtool network <name> on <host>. A network vmtool did not
make (its bridge is not vmt- plus six hex digits), and one with VMs attached,
are refused.`,
		Args: cobra.ExactArgs(2),
	}
	cmd.RunE = withNetHost(e, func(m app.Manager, cmd *cobra.Command, args []string) error {
		if err := e.service("").DeleteNetwork(cmd.Context(), m, args[1]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Network %q deleted from %s\n", args[1], args[0])
		return nil
	})
	return cmd
}

func netShowCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <host> <name>",
		Short: "Show one libvirt network on a host",
		Args:  cobra.ExactArgs(2),
	}
	cmd.RunE = withNetHost(e, func(m app.Manager, cmd *cobra.Command, args []string) error {
		n, err := e.service("").Network(cmd.Context(), m, args[1])
		if err != nil {
			return err
		}
		writeNetwork(cmd.OutOrStdout(), n)
		return nil
	})
	return cmd
}

// withNetHost resolves args[0] as a host this vmtool serves, then runs fn on
// its Manager. An unknown host fails before any Manager opens.
func withNetHost(e *env, fn func(m app.Manager, cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		m, release, err := e.resolver().Resolve(args[0])
		if err != nil {
			return err
		}
		defer release()
		return fn(m, cmd, args)
	}
}

func writeNetwork(w io.Writer, n *vmtool.NetworkInfo) {
	kind := "not made by vmtool"
	if n.Vmtool {
		kind = fmt.Sprintf("vmtool (VNI %d)", n.VNI)
	}
	state := "inactive"
	if n.Active {
		state = "active"
	}
	if n.Autostart {
		state += ", autostart"
	}
	rows := [][2]string{
		{"name", n.Name}, {"kind", kind}, {"state", state}, {"bridge", n.Bridge}, {"forward", n.Forward},
		{"subnet", n.Subnet}, {"gateway", n.Gateway}, {"domain", n.Domain},
	}
	if n.MTU > 0 {
		rows = append(rows, [2]string{"mtu", fmt.Sprint(n.MTU)})
	}
	if n.DHCPStart != "" {
		rows = append(rows, [2]string{"dhcp", n.DHCPStart + " - " + n.DHCPEnd})
	}
	for _, r := range rows {
		if r[1] != "" {
			fmt.Fprintf(w, "%-8s %s\n", r[0]+":", r[1])
		}
	}
	for i, vm := range n.VMs {
		label := ""
		if i == 0 {
			label = "vms:"
		}
		fmt.Fprintf(w, "%-8s %s\n", label, vm)
	}
	for i, r := range n.Reservations {
		label := ""
		if i == 0 {
			label = "reserved:"
		}
		fmt.Fprintf(w, "%-9s %s %s %s\n", label, r.IP, r.MAC, r.Name)
	}
}

func netJoinCmd(e *env) *cobra.Command {
	var home string
	cmd := &cobra.Command{
		Use:   "join <host> <name>",
		Short: "Make a host a member of a vmtool network, so its VMs share it",
		Long: `Make <host> a member of the vmtool network <name>: the network's home (the
host whose libvirt network does the NAT, DHCP and DNS) keeps doing that, and
<host> defines the same network with only a bridge and the MTU (no address of
its own) and links to the home and to every other member with a VXLAN device
(UDP port 4789 over the LAN). Attach a VM's card to it on <host> as on the home:
the address and the name come from the home's DHCP.

Joining needs the store: a network that spans hosts is registered there, with
its members' LAN addresses. A network made without a store (bridge number 1 to
9999) is registered by its first join, with its bridge number as its VNI, if
the registry does not have that number yet (else: recreate it with a store);
say which host is its home with --home. Only <host> (and, the first time, the
home) must be awake; every other member that answers gets the new peer list
and one that does not is skipped with a line: it reads the list from the store
when its daemon starts.

VXLAN is neither authenticated nor encrypted: anyone on the LAN can read the
network's traffic and inject frames into it. Use it on a LAN you trust. The
hosts' firewalls must pass UDP 4789 between them (just prereqs opens it from
the LAN when ufw is active).

  vmtool net join hv2 net1
  vmtool net join hv2 lab --home hv1   # a network made without a store`,
		Args: cobra.ExactArgs(2),
	}
	cmd.Flags().StringVar(&home, "home", "", "the network's home host; needed only when the network is not in the registry yet")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		res, err := e.service("").JoinNetwork(cmd.Context(), args[0], args[1], home)
		if err != nil {
			return err
		}
		writeJoin(cmd.OutOrStdout(), res)
		return nil
	}
	return cmd
}

func netLeaveCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "leave <host> <name>",
		Short: "Take a host out of a vmtool network",
		Long: `Take <host> out of the vmtool network <name>: it unlinks, removes the member
network and its registry entry, and every other member that answers gets the
shorter peer list. Refused while VMs of <host> have a card on the network
(detach it first: vmtool net detach), and for the network's home (delete the
network instead). Needs the store.`,
		Args: cobra.ExactArgs(2),
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		res, err := e.service("").LeaveNetwork(cmd.Context(), args[0], args[1])
		if err != nil {
			return err
		}
		writeLeave(cmd.OutOrStdout(), res)
		return nil
	}
	return cmd
}

func writeJoin(w io.Writer, r *app.JoinResult) {
	fmt.Fprintf(w, "Host %s joined network %q (VNI %d, home %s)\n", r.Host, r.Network, r.VNI, r.Home)
	if r.Registered {
		fmt.Fprintf(w, "Network %q was made without a store: it is registered now, with its bridge number %d as its VNI\n", r.Network, r.VNI)
	}
	fmt.Fprintf(w, "Link:      %s (%s)\n", r.LanIP, r.LanDev)
	if len(r.Peers) == 0 {
		fmt.Fprintln(w, "Peers:     no other host yet")
	} else {
		fmt.Fprintf(w, "Peers:     %s\n", strings.Join(r.Peers, ", "))
	}
	for _, l := range r.Skipped {
		fmt.Fprintln(w, "Skipped:  ", l)
	}
}

func writeLeave(w io.Writer, r *app.LeaveResult) {
	fmt.Fprintf(w, "Host %s left network %q\n", r.Host, r.Network)
	for _, l := range r.Skipped {
		fmt.Fprintln(w, "Skipped:  ", l)
	}
}
