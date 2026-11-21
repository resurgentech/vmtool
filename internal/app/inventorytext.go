package app

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jaredeh/vmtool/internal/storeclient"
)

// InventoryVM is one VM row of the inventory.
type InventoryVM = storeclient.InventoryVM

// The plain-text form of the inventory, shared by the CLI and the TUI so the
// two cannot drift: hosts, then VMs, then machines. Times are plain UTC
// timestamps (the store never judges whether a host is up, so there is no age
// and no staleness mark).

// oneLn collapses user text (a comment) to one line of single-spaced words.
func oneLn(s string) string { return strings.Join(strings.Fields(s), " ") }

// invTime is a plain timestamp: UTC, no age, no staleness.
func invTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func invDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// invGiB writes a byte count in GiB: whole when it is, one decimal otherwise.
func invGiB(b int64) string {
	const gib = int64(1) << 30
	if b%gib == 0 {
		return fmt.Sprintf("%dG", b/gib)
	}
	return fmt.Sprintf("%.1fG", float64(b)/float64(gib))
}

func invDisks(v InventoryVM) string {
	if len(v.Disks) == 0 {
		return "-"
	}
	parts := make([]string, len(v.Disks))
	for i, d := range v.Disks {
		parts[i] = fmt.Sprintf("%s %s %s", d.Target, invGiB(d.CapacityBytes), d.Kind)
	}
	return strings.Join(parts, ", ")
}

// invOrigin is the image (or template) and clone kind: "img1 (thin)".
func invOrigin(v InventoryVM) string {
	o := v.Origin
	if o == nil {
		return "-"
	}
	name := ""
	switch {
	case o.Image != nil && *o.Image != "":
		name = *o.Image
	case o.Template != nil && *o.Template != "":
		name = *o.Template
	}
	kind := ""
	if o.Clone != nil {
		kind = *o.Clone
	}
	switch {
	case name != "" && kind != "":
		return name + " (" + kind + ")"
	case name != "":
		return name
	case kind != "":
		return "(" + kind + ")"
	}
	return "-"
}

func invCreated(v InventoryVM) string {
	if v.Origin == nil || v.Origin.Created == nil {
		return "-"
	}
	return invDash(*v.Origin.Created)
}

// WriteInventory writes hosts, then VMs, then machines. Names and comments
// are written on one line each (oneLine): a comment is user text.
func WriteInventory(out io.Writer, inv *Inventory, gone bool) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "HOSTS")
	fmt.Fprintln(w, "NAME\tLAST REPORT\tDAEMON\tLIVE\tGONE")
	for _, h := range inv.Hosts {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\n", oneLn(h.Name), invTime(h.LastReport), invDash(oneLn(h.DaemonVersion)), h.Live, h.Gone)
	}
	if len(inv.Hosts) == 0 {
		fmt.Fprintln(w, "(none)")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "VMS")
	head := "VM\tSTATE\tVCPUS\tMEMORY\tIP\tDISKS\tORIGIN\tCREATED\tCOMMENT\tLAST SEEN"
	if gone {
		head += "\tGONE SINCE\tKEPT UNTIL"
	}
	fmt.Fprintln(w, head)
	for _, v := range inv.Vms {
		row := fmt.Sprintf("%s/%s\t%s\t%d\t%dG\t%s\t%s\t%s\t%s\t%s\t%s",
			oneLn(v.Host), oneLn(v.Name), invDash(oneLn(v.State)), v.Vcpus, v.Memory, invDash(oneLn(v.Ip)),
			oneLn(invDisks(v)), oneLn(invOrigin(v)), oneLn(invCreated(v)), invDash(oneLn(v.Comment)), invTime(v.LastSeen))
		if gone {
			gs, ku := "-", "-"
			if v.GoneSince != nil {
				gs = invTime(*v.GoneSince)
			}
			if v.CredentialsKeptUntil != nil {
				ku = invTime(*v.CredentialsKeptUntil)
			}
			row += "\t" + gs + "\t" + ku
		}
		fmt.Fprintln(w, row)
	}
	if len(inv.Vms) == 0 {
		fmt.Fprintln(w, "(none)")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "MACHINES")
	fmt.Fprintln(w, "NAME\tKIND")
	for _, m := range inv.Machines {
		fmt.Fprintf(w, "%s\t%s\n", oneLn(m.Name), oneLn(m.Kind))
	}
	if len(inv.Machines) == 0 {
		fmt.Fprintln(w, "(none)")
	}
	return w.Flush()
}
