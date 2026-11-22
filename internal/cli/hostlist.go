package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/resurgentech/vmtool/internal/api"
	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/hosts"
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
and does not change the exit status: the listing ran. A network home (hosts:
vms: false) is not asked: it is one row, "<host>  network home: runs no VMs".
Name a host to list only that host (a network home too); then it is asked and
its failure is the command's error.`,
		Example: `  vmtool list
  vmtool list hv1
  vmtool list --json

  Tip: with --json stdout is {"vms", "errors", "network_homes"}: each VM is the
  API's VMInfo with its host; an unreachable host is a {host, error} row under
  errors, and the exit status stays 0.`,
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
			// A host named explicitly is asked, a network home included.
			home := r.NetworkHome
			if len(args) == 1 {
				home = nil
			}
			rows := app.ListAll(cmd.Context(), names, r.Resolve, home, app.ListTimeout)
			if len(args) == 1 && rows[0].Err != nil {
				return rows[0].Err
			}
			rows = leaveOutOwnName(cmd.ErrOrStderr(), r, rows)
			if jsonMode(cmd) {
				return emit(cmd, listObject(rows, r.NetworkHome))
			}
			return printVMTable(cmd.OutOrStdout(), rows, r.NetworkHome)
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
// per host that did not answer, and one row for a network home (a host that
// runs no VMs) that has none to list.
func printVMTable(out io.Writer, rows []app.HostVMs, networkHome func(string) bool) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE\tVCPUS\tMEMORY\tAUTOSTART\tIP")
	for _, h := range rows {
		if h.Err != nil {
			fmt.Fprintf(w, "%s\tERROR: %s\n", h.Host, oneLine(h.Err.Error()))
			continue
		}
		if h.NetworkHome || len(h.VMs) == 0 && networkHome(h.Host) {
			fmt.Fprintf(w, "%s\tnetwork home: runs no VMs\n", h.Host)
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

// listJSON is `list --json`: the VMs of every host that answered (the API's
// VMInfo and the host), the hosts that did not as {host, error} rows, and the
// network homes (hosts that run no VMs).
type listJSON struct {
	VMs          []listVMJSON  `json:"vms"`
	Errors       []hostErrJSON `json:"errors"`
	NetworkHomes []string      `json:"network_homes"`
}

type listVMJSON struct {
	Host string `json:"host"`
	api.VMInfo
}

type hostErrJSON struct {
	Host  string `json:"host"`
	Error string `json:"error"`
}

func listObject(rows []app.HostVMs, networkHome func(string) bool) listJSON {
	out := listJSON{VMs: []listVMJSON{}, Errors: []hostErrJSON{}, NetworkHomes: []string{}}
	for _, h := range rows {
		switch {
		case h.Err != nil:
			out.Errors = append(out.Errors, hostErrJSON{Host: h.Host, Error: oneLine(h.Err.Error())})
		case h.NetworkHome || len(h.VMs) == 0 && networkHome != nil && networkHome(h.Host):
			out.NetworkHomes = append(out.NetworkHomes, h.Host)
		default:
			for i := range h.VMs {
				out.VMs = append(out.VMs, listVMJSON{Host: h.Host, VMInfo: apiVMInfo(&h.VMs[i])})
			}
		}
	}
	return out
}

// hostsJSON is `hosts --json`: a row per host, and the store's row when a
// store is configured.
type hostsJSON struct {
	Hosts []hostRowJSON `json:"hosts"`
	Store *storeRowJSON `json:"store,omitempty"`
}

type hostRowJSON struct {
	Name         string `json:"name"`
	Address      string `json:"address,omitempty"`
	Local        bool   `json:"local"`
	NetworkHome  bool   `json:"network_home"`
	Reachable    bool   `json:"reachable"`
	APIVersion   string `json:"api_version,omitempty"`
	CertDaysLeft *int   `json:"cert_days_left,omitempty"`
	CertNotAfter string `json:"cert_not_after,omitempty"`
	Error        string `json:"error,omitempty"`
}

type storeRowJSON struct {
	URL       string `json:"url"`
	Reachable bool   `json:"reachable"`
	Caller    string `json:"caller,omitempty"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

// hostsObject builds the object; the certificate warnings (what the table
// prints under it) are returned apart, for stderr.
func hostsObject(st []app.HostStatus, now time.Time) (hostsJSON, []string) {
	out := hostsJSON{Hosts: []hostRowJSON{}}
	var warnings []string
	for _, s := range st {
		row := hostRowJSON{Name: s.Host, Address: s.Address, Local: s.Local, NetworkHome: s.NetworkHome,
			Reachable: s.Reachable, APIVersion: s.APIVersion}
		if d, ok := s.CertDaysLeft(now); ok {
			row.CertDaysLeft = &d
			row.CertNotAfter = s.CertNotAfter.UTC().Format(time.RFC3339)
			if d < certWarnDays {
				warnings = append(warnings, certWarning(s, d))
			}
		}
		if s.Err != nil {
			row.Error = oneLine(s.Err.Error())
		}
		out.Hosts = append(out.Hosts, row)
	}
	return out, warnings
}

func storeObject(st app.StoreStatus) *storeRowJSON {
	row := &storeRowJSON{URL: st.URL, Reachable: st.Reachable, Caller: st.Caller, Version: st.Version}
	if st.Err != nil {
		row.Error = oneLine(st.Err.Error())
	}
	return row
}

func hostsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "hosts",
		Short: "Show every host: address, reachability, API version, certificate days left",
		Long: `Probe this host and every configured host (GET /version over TLS, 5 s timeout,
concurrently) and print name, address, whether it answered, the API version it
reports (informational, never compared) and the days left on the certificate it presented. A warning line
follows for each certificate with fewer than 30 days left. A network home (hosts:
vms: false) is written "<host> (network home)".`,
		Example: `  vmtool hosts
  vmtool hosts --json

  Tip: with --json stdout is {"hosts", "store"} (store only when one is
  configured); certificate warnings go to stderr.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st := e.resolver().Status(cmd.Context(), app.ListTimeout)
			if jsonMode(cmd) {
				obj, warnings := hostsObject(st, nowFn())
				for _, w := range warnings {
					fmt.Fprintln(cmd.ErrOrStderr(), w)
				}
				if sess := e.session(); sess != nil {
					obj.Store = storeObject(sess.Status(cmd.Context(), app.WhoamiTimeout))
				}
				return emit(cmd, obj)
			}
			if err := printHosts(cmd.OutOrStdout(), st, nowFn()); err != nil {
				return err
			}
			if sess := e.session(); sess != nil {
				return printStore(cmd.OutOrStdout(), sess.Status(cmd.Context(), app.WhoamiTimeout))
			}
			return nil
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
		name := s.Host
		if s.NetworkHome {
			name += " (network home)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", name, addr, reach, api, days, msg)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, line := range warnings {
		fmt.Fprintln(out, line)
	}
	return nil
}

// printStore writes the store's row, after a blank line: whether it answered,
// the name it gave this token, and its version. An unreachable store is a row,
// not a failure.
func printStore(out io.Writer, st app.StoreStatus) error {
	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tURL\tREACHABLE\tCALLER\tVERSION\tERROR")
	reach := "no"
	if st.Reachable {
		reach = "yes"
	}
	caller, version, msg := "-", "-", "-"
	if st.Caller != "" {
		caller = st.Caller
	}
	if st.Version != "" {
		version = st.Version
	}
	if st.Err != nil {
		msg = oneLine(st.Err.Error())
	}
	fmt.Fprintf(w, "store\t%s\t%s\t%s\t%s\t%s\n", st.URL, reach, caller, version, msg)
	return w.Flush()
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
