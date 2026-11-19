package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

func templatesCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "templates",
		Short: "Manage the thin-pool templates of a hypervisor",
		Long: `A thin pool holds one template per (image, SHA-256), built from the host's cached
image the first time a VM is created into the pool; a clone is a thin snapshot
of it. Templates are removed only here. A stale template is an image template
whose image is no longer the host's current copy under that name. Removing a
template never touches its clones.`,
	}
	cmd.AddCommand(templatesListCmd(e), templatesPrepareCmd(e), templatesDeleteCmd(e), templatesPruneCmd(e))
	return cmd
}

func templatesListCmd(e *env) *cobra.Command {
	var host string
	var stale bool
	cmd := &cobra.Command{
		Use:     "list",
		Short:   "List templates on every host (or one), named host/name",
		Aliases: []string{"ls"},
		Long: `List the templates of this host and every configured host, each queried
concurrently with a 5 s timeout. An unreachable host is one row,
"<host>  ERROR: <why>". With --host, only that host, and its failure is the
command's error. A host with no thin pools has no rows. --stale lists only
stale templates. SHA256 shows the first 12 hex digits, which are also the
suffix of the template's LV name (vmtpl-<12 hex>).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := e.resolver()
			names := r.AllNames()
			if host != "" {
				if err := r.Check(host); err != nil {
					return err
				}
				names = []string{host}
			}
			rows := e.service("").ListAllTemplates(cmd.Context(), names, r.Resolve, app.ListTimeout)
			if host != "" && rows[0].Err != nil {
				return rows[0].Err
			}
			rows = leaveOutOwnNameTemplates(cmd.ErrOrStderr(), e, rows)
			return writeTemplateTable(cmd.OutOrStdout(), rows, stale)
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "list only this host's templates")
	cmd.Flags().BoolVar(&stale, "stale", false, "list only stale templates")
	return cmd
}

// leaveOutOwnNameTemplates is leaveOutOwnName for template rows.
func leaveOutOwnNameTemplates(stderr io.Writer, e *env, rows []app.HostTemplates) []app.HostTemplates {
	r := e.resolver()
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

// writeTemplateTable prints the combined listing: one row per template, one
// ERROR row per host that did not answer. Only stale rows with stale set.
func writeTemplateTable(out io.Writer, rows []app.HostTemplates, stale bool) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tORIGIN\tIMAGE\tSHA256\tPOOL\tSIZE\tUSED\tCLONES\tSTALE\tCREATED")
	for _, h := range rows {
		if h.Err != nil {
			fmt.Fprintf(w, "%s\tERROR: %s\n", h.Host, oneLine(h.Err.Error()))
			continue
		}
		for _, t := range h.Templates {
			if stale && !t.Stale {
				continue
			}
			image, sha := dash(t.Image), dash(shortSHA(t.SHA256))
			isStale := "no"
			if t.Stale {
				isStale = "yes"
			}
			created := "-"
			if !t.Created.IsZero() {
				created = t.Created.UTC().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", h.Host, t.Name, t.Origin, image, sha, t.Pool,
				formatPoolBytes(t.SizeBytes), formatPoolBytes(t.UsedBytes), t.Clones, isStale, created)
		}
	}
	return w.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func templatesPrepareCmd(e *env) *cobra.Command {
	var pool string
	cmd := &cobra.Command{
		Use:   "prepare <host> <image>",
		Short: "Build an image template on a host ahead of the first create",
		Long: `Build the template of <image> in a thin pool of <host> now, so the first create
into the pool does not pay for it. The image is first put on the host exactly as
create does (the store's copy, a relay, or an upload from this machine); then
the host builds the template from its cached copy. A template that already
exists is left as it is. Without --pool the host's default pool is used.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			host, image := args[0], args[1]
			r := e.resolver()
			m, release, err := r.Resolve(host)
			if err != nil {
				return err
			}
			defer release()
			errw := cmd.ErrOrStderr()
			t, err := e.service("").PrepareTemplate(cmd.Context(), m, app.PrepareTemplateInput{
				Host: host, Image: image, Pool: pool,
				OnProgress: func(ev app.ProgressEvent) {
					if uploadProgress(errw, ev) {
						return
					}
					if ev.Status == "done" && ev.Detail != "" {
						fmt.Fprintln(errw, ev.Detail)
					}
				},
			})
			if err != nil {
				return printAppErr(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Template %s/%s is ready in pool %s (%s, %s, %d clones)\n",
				host, t.Name, t.Pool, t.LV, formatPoolBytes(t.SizeBytes), t.Clones)
			return nil
		},
	}
	cmd.Flags().StringVar(&pool, "pool", "", "thin pool (default: the host's default pool)")
	return cmd
}

// parseTemplateRef splits host/<name>. Both parts must be plain names; the
// host is checked against the hosts this vmtool serves by Resolve.
func parseTemplateRef(s string) (host, name string, err error) {
	host, name, ok := strings.Cut(s, "/")
	if !ok || !vmtool.ValidMachineName(host) || !vmtool.ValidImageName(name) {
		return "", "", fmt.Errorf("%q: want host/<template name> (or host/<LV name>, vmtpl-<12 hex>), each letters, digits, '.', '_', '-'", s)
	}
	return host, name, nil
}

func templatesDeleteCmd(e *env) *cobra.Command {
	var pool string
	cmd := &cobra.Command{
		Use:     "delete <host>/<name>",
		Short:   "Remove one template; the VMs cloned from it keep working",
		Aliases: []string{"rm"},
		Long: `Remove the template <name> (its name, or its LV name vmtpl-<12 hex> as the list
shows in SHA256) from <host>. Its clones are independent snapshots and are not
touched. When <name> matches more than one template (a current and a stale one
of the same image, or the same name in two pools) nothing is removed and the
candidates are listed: repeat with the LV name, and --pool if it is in two.
A name that is not a template is not found; a VM's volume is never removed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host, name, err := parseTemplateRef(args[0])
			if err != nil {
				return err
			}
			m, release, err := e.resolver().Resolve(host)
			if err != nil {
				return err
			}
			defer release()
			t, err := e.service("").DeleteTemplate(cmd.Context(), m, name, pool)
			if err != nil {
				return printAppErr(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Template %s/%s (%s in pool %s) removed; its %d clones are unaffected\n", host, t.Name, t.LV, t.Pool, t.Clones)
			return nil
		},
	}
	cmd.Flags().StringVar(&pool, "pool", "", "only the template in this thin pool")
	return cmd
}

func templatesPruneCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "prune <host>",
		Short: "Remove every stale template on a host",
		Long: `Remove the stale image templates of <host>: those whose image is no longer the
host's current copy under that name. Current templates and saved templates are
never removed, and neither are any clones.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := args[0]
			m, release, err := e.resolver().Resolve(host)
			if err != nil {
				return err
			}
			defer release()
			removed, err := e.service("").PruneTemplates(cmd.Context(), m)
			out := cmd.OutOrStdout()
			for _, t := range removed {
				fmt.Fprintf(out, "removed %s/%s (%s in pool %s)\n", host, t.Name, t.LV, t.Pool)
			}
			if err != nil {
				return printAppErr(err)
			}
			if len(removed) == 0 {
				fmt.Fprintf(out, "No stale templates on %s\n", host)
				return nil
			}
			fmt.Fprintf(out, "Removed %d stale templates from %s\n", len(removed), host)
			return nil
		},
	}
}
