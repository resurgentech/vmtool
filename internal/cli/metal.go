package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"text/tabwriter"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/spf13/cobra"
)

func metalCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "metal",
		Short: "Manage metal and alien machines (SSH/Ansible targets outside libvirt)",
		Long: `Metal machines are physical boxes; aliens are VMs this host's libvirt
cannot see. Both are registered in .machines/<name>/machine.yml and must run
an Ubuntu-compatible OS. Onboarding needs SSH as packer/packer with
passwordless sudo; ansible/playbooks/prep_machine.yml sets that up.`,
	}
	cmd.AddCommand(
		metalAddCmd(e),
		metalListCmd(e),
		metalInfoCmd(e),
		metalRmCmd(e),
		metalSSHCmd(e),
		metalExecCmd(e),
		metalPlaybookCmd(e),
		metalRebootCmd(e),
	)
	return cmd
}

func metalAddCmd(e *env) *cobra.Command {
	var (
		kind       string
		commission bool
	)
	cmd := &cobra.Command{
		Use:   "add <name> <host>",
		Short: "Onboard a machine: verify packer SSH, register, run setup_bot.yml",
		Long: `Check that <host> accepts SSH as packer/packer with passwordless sudo and
runs an Ubuntu-compatible OS, write .machines/<name>/machine.yml, then run
setup_bot.yml (bot user, key auth; password login disabled for bot only).

Re-running with the same host and kind resumes a failed onboarding.

  vmtool metal add nuc1 10.0.0.42
  vmtool metal add cloud1 203.0.113.7 --kind alien --commission`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// libvirt is only needed to refuse VM names; machines work without it.
			vms, release := e.libvirtNameCheck()
			defer release()
			info, err := e.service("").AddMachine(cmd.Context(), vms, app.AddMachineInput{
				Name:       args[0],
				Host:       args[1],
				Kind:       kind,
				Commission: commission,
				OnProgress: func(ev app.ProgressEvent) {
					if ev.Output != "" {
						fmt.Print(ev.Output)
					}
					if ev.Detail != "" {
						fmt.Println(ev.Detail)
					}
				},
			})
			if err != nil {
				return err
			}
			fmt.Printf("%s %q onboarded at %s (%s)\n", info.Kind, info.Name, info.Host, info.OS)
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "metal", "metal or alien")
	cmd.Flags().BoolVar(&commission, "commission", false, "alien only: run commission.yml first (new hostname, machine-id, host keys; reboots)")
	return cmd
}

func metalListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List machines and probe them over SSH",
		Aliases: []string{"ls"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ms, err := e.service("").ListMachines(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKIND\tSTATE\tHOST\tOS")
			for _, m := range ms {
				osName := m.OS
				if osName == "" {
					osName = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.Name, m.Kind, m.State, m.Host, osName)
			}
			return w.Flush()
		},
	}
}

func metalInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "info <name>",
		Short: "Show a machine and probe it over SSH",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := e.service("").GetMachine(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Printf("Name:   %s\n", m.Name)
			fmt.Printf("Kind:   %s\n", m.Kind)
			fmt.Printf("Host:   %s\n", m.Host)
			fmt.Printf("State:  %s\n", m.State)
			if m.OS != "" {
				fmt.Printf("OS:     %s\n", m.OS)
			}
			if m.Error != "" {
				fmt.Printf("Error:  %s\n", m.Error)
			}
			return nil
		},
	}
}

func metalRmCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Forget a machine (removes .machines/<name>; the machine is untouched)",
		Aliases: []string{"remove"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := e.service("").RemoveMachine(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("Machine %q forgotten\n", args[0])
			return nil
		},
	}
}

func metalSSHCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh <name> [-- ssh-args...]",
		Short: "SSH into a machine using its .machines credentials",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := e.service("").MachineSSHCmd(args[0], args[1:]...)
			if err != nil {
				return err
			}
			return c.Run()
		},
	}
}

func metalExecCmd(e *env) *cobra.Command {
	var tty bool
	cmd := &cobra.Command{
		Use:   "exec <name> [--] <arg>...",
		Short: "Run a program on a machine over SSH",
		Long: `Run a program and pass through stdin, stdout, stderr, and the remote
exit code. The arguments are the exact argv (no shell parsing); for shell
features, run a shell. -- right after the target ends vmtool's flags and is
not sent; everything after it is the command, exactly.

  vmtool metal exec nuc1 uname -a
  vmtool metal exec nuc1 -- sh -c 'echo hi | wc -c'

-t / --tty gives the command a live terminal, like ssh -t: your terminal goes
into raw mode and its size and resizes are forwarded. Without a terminal on
stdin (a pipe or a file) -t still forces a remote pty: the program sees a
terminal, stdout and stderr are merged, input is echoed. Put -t before the
machine; after it, -t belongs to the remote command.

  vmtool metal exec -t nuc1 -- top`,
		Args:          execArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := e.service("").MachineRemoteCmd(args[0], execCommand(args), ttyOpts(tty)...)
			if err != nil {
				return err
			}
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			err = c.Run()
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return remoteExitError{code: ee.ExitCode()}
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "force a remote terminal (ssh -tt) and attach yours live")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

func metalPlaybookCmd(e *env) *cobra.Command {
	var extraVars []string
	cmd := &cobra.Command{
		Use:   "playbook <name> <playbook>",
		Short: "Run an Ansible playbook against a machine",
		Long: `Run a playbook (bare name under ansible/playbooks, or a path). deviceid
and vmtool_kind default to the machine's name and kind. commission.yml is
refused; use 'metal add --kind alien --commission'.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			extra, err := parseExtraVars(extraVars)
			if err != nil {
				return err
			}
			res, err := e.service("").RunMachinePlaybook(cmd.Context(), args[0], args[1], extra)
			if res != nil && res.Output != "" {
				fmt.Print(res.Output)
			}
			if err != nil {
				return err
			}
			fmt.Printf("Playbook %s completed\n", args[1])
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&extraVars, "extra-var", nil, "ansible extra-var key=value (repeatable; name: letters, digits, _). Values go to ansible in its environment, not argv")
	return cmd
}

func metalRebootCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "reboot <name>",
		Short: "Reboot a machine over SSH (there is no power-off or start)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := e.service("").RebootMachine(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Printf("Machine %q rebooting\n", args[0])
			return nil
		},
	}
}
