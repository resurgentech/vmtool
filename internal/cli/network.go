package cli

import (
	"fmt"
	"io"

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
what marks it as vmtool's. vmtool leaves every other libvirt network alone.`,
	}
	cmd.AddCommand(netCreateCmd(e), netDeleteCmd(e), netShowCmd(e))
	return cmd
}

func netCreateCmd(e *env) *cobra.Command {
	var subnet, domain string
	cmd := &cobra.Command{
		Use:   "create <host> <name>",
		Short: "Make a network on a host",
		Long: `Make a vmtool network called <name> on <host>: NAT, DHCP (.100 to .200), DNS
and autostart. The subnet is --subnet, or the first /24 of 10.77.0.0/16 that no
libvirt network or route on the host uses; one that overlaps is refused. The
DNS domain is --domain, or <name>.test. <name> follows the VM name rule and is
never "default".`,
		Args: cobra.ExactArgs(2),
	}
	cmd.Flags().StringVar(&subnet, "subnet", "", "the network's /24, such as 10.77.1.0/24 (default: the first free /24 of 10.77.0.0/16)")
	cmd.Flags().StringVar(&domain, "domain", "", "DNS domain (default: <name>.test)")
	cmd.RunE = withNetHost(e, func(m app.Manager, cmd *cobra.Command, args []string) error {
		n, err := e.service("").CreateNetwork(cmd.Context(), m, args[1], subnet, domain)
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
}
