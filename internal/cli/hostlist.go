package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/spf13/cobra"
)

// nowFn is the clock for certificate days; tests replace it.
var nowFn = time.Now

// certWarnDays is the threshold under which `vmtool hosts` warns.
const certWarnDays = 30

// oneLine collapses a message to a single line of single-spaced words.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func listCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list [host]",
		Short: "List VMs on every host (or one), named host/vm",
		Long: `List VMs on this host and every configured host, each queried concurrently
with a 5 s timeout. An unreachable host is one row, "<host>  ERROR: <why>",
and does not change the exit status: the listing ran. Name a host to list only
that host; then its failure is the command's error.`,
		Aliases: []string{"ls"},
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := e.resolver()
			names := r.AllNames()
			if len(args) == 1 {
				if err := r.Check(args[0]); err != nil {
					return err
				}
				names = args[0:1]
			}
			rows := app.ListAll(cmd.Context(), names, r.Resolve, app.ListTimeout)
			if len(args) == 1 && rows[0].Err != nil {
				return rows[0].Err
			}
			rows = leaveOutOwnName(cmd.ErrOrStderr(), r, rows)
			return printVMTable(cmd.OutOrStdout(), rows)
		},
	}
}

// noLocalBackend is how Resolve words the own name's failure when it has no
// daemon listed and local libvirt cannot be opened (internal/hosts).
const noLocalBackend = "no daemon listed for "

// leaveOutOwnName drops the own name's row when it has neither a daemon nor
// local libvirt and says so once on stderr: the listing ran for every host
// that can answer, so the exit status is unchanged. Any other failure of the
// own name stays an ERROR row.
func leaveOutOwnName(stderr io.Writer, r *hosts.Resolver, rows []app.HostVMs) []app.HostVMs {
	out := rows[:0:0]
	for _, h := range rows {
		if h.Host == r.Local() && !r.OwnListed() && h.Err != nil && strings.HasPrefix(h.Err.Error(), noLocalBackend) {
			fmt.Fprintf(stderr, "note: %s; %s left out of this listing\n", oneLine(h.Err.Error()), h.Host)
			continue
		}
		out = append(out, h)
	}
	return out
}

// printVMTable writes the combined listing: one row per VM, one ERROR row
// per host that did not answer.
func printVMTable(out io.Writer, rows []app.HostVMs) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tVCPUS\tMEMORY\tAUTOSTART\tIP")
	for _, h := range rows {
		if h.Err != nil {
			fmt.Fprintf(w, "%s\tERROR: %s\n", h.Host, oneLine(h.Err.Error()))
			continue
		}
		for _, vm := range h.VMs {
			ip := vm.IP
			if ip == "" {
				ip = "-"
			}
			autostart := "off"
			if vm.Autostart {
				autostart = "on"
			}
			name := app.VMRef{Host: h.Host, Name: vm.Name}.String()
			fmt.Fprintf(w, "%s\t%s\t%d\t%d GiB\t%s\t%s\n", name, vm.State, vm.VCPUs, vm.Memory, autostart, ip)
		}
	}
	return w.Flush()
}

func hostsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "hosts",
		Short: "Show every host: address, reachability, API version, certificate days left",
		Long: `Probe this host and every configured host (GET /version over TLS, 5 s timeout,
concurrently) and print name, address, whether it answered, the API version it
reports (informational, never compared) and the days left on the certificate it presented. A warning line
follows for each certificate with fewer than 30 days left.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st := e.resolver().Status(cmd.Context(), app.ListTimeout)
			return printHosts(cmd.OutOrStdout(), st, nowFn())
		},
	}
}

func printHosts(out io.Writer, st []app.HostStatus, now time.Time) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tADDRESS\tREACHABLE\tAPI\tCERT_DAYS\tERROR")
	var warnings []string
	for _, s := range st {
		reach := "no"
		if s.Reachable {
			reach = "yes"
		}
		api, days, msg := "-", "-", "-"
		if s.APIVersion != "" {
			api = s.APIVersion
		}
		if d, ok := s.CertDaysLeft(now); ok {
			days = fmt.Sprint(d)
			if d < certWarnDays {
				warnings = append(warnings, certWarning(s, d))
			}
		}
		if s.Err != nil {
			msg = oneLine(s.Err.Error())
		}
		addr := s.Address
		if addr == "" {
			addr = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Host, addr, reach, api, days, msg)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, line := range warnings {
		fmt.Fprintln(out, line)
	}
	return nil
}

func certWarning(s app.HostStatus, days int) string {
	fix := fmt.Sprintf("run 'just cert %s' and 'just deploy %s'", s.Host, s.Host)
	if s.Local {
		fix = "run 'just install'"
	}
	date := s.CertNotAfter.UTC().Format("2006-01-02")
	if days < 0 {
		return fmt.Sprintf("warning: %s certificate expired on %s; %s", s.Host, date, fix)
	}
	return fmt.Sprintf("warning: %s certificate expires in %d days (%s); %s", s.Host, days, date, fix)
}
