package cli

import (
	"fmt"
	"io"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/spf13/cobra"
)

// machinesCmd is the `vmtool machines` group: housekeeping of .machines/.
// Metal/alien machines themselves are managed under `vmtool metal`.
func machinesCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "machines",
		Short: "Maintain .machines/ (VM credentials and machine registrations)",
	}
	cmd.AddCommand(machinesMigrateCmd(e))
	return cmd
}

func machinesMigrateCmd(e *env) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Move pre-host VM credentials to .machines/<host>/<vm>/",
		Long: `VM credentials now live in .machines/<host>/<vm>/. Older vmtool kept them in
.machines/<vm>/ or .machines/<ip>/. For each .machines/<x>/ without a
machine.yml (metal/alien machines are left alone) and that is not a host
directory:

  x is a VM on this host        -> .machines/<this host>/<x>/
  x is an IP a VM here holds now -> .machines/<this host>/<that vm>/
  anything else                  -> reported and left in place

Nothing is ever overwritten. Without --apply this only prints the plan.`,
		Args: cobra.NoArgs,
		RunE: withManager(e, func(m app.Manager, cmd *cobra.Command, _ []string) error {
			svc := e.service("")
			local := e.resolver().Local()
			steps, err := svc.PlanMigration(cmd.Context(), m, local)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			printPlan(out, steps, apply)
			if !apply {
				return nil
			}
			done, err := svc.ApplyMigration(cmd.Context(), steps)
			for _, st := range done {
				fmt.Fprintf(out, "moved %s -> %s\n", st.Source, st.Target)
			}
			return err
		}),
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "perform the moves (default: dry run)")
	return cmd
}

func printPlan(w io.Writer, steps []app.MigrationStep, apply bool) {
	if len(steps) == 0 {
		fmt.Fprintln(w, "nothing to migrate")
		return
	}
	moves := 0
	for _, st := range steps {
		if st.Move {
			moves++
			if !apply {
				fmt.Fprintf(w, "would move %s -> %s\n", st.Source, st.Target)
			}
			continue
		}
		fmt.Fprintf(w, "leave %s: %s\n", st.Source, st.Reason)
	}
	if !apply && moves > 0 {
		fmt.Fprintln(w, "dry run; re-run with --apply to move")
	}
}
