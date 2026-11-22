package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// The templates view (T from the VM list): every host's thin-pool templates,
// one row each, with the stale mark; D removes the selected one after the same
// confirm the VM delete uses. A template's clones are independent snapshots,
// so a removal never touches a VM.

// tplRow is one template row: the template and the host it is on. A host
// that did not answer is a row with Err set and no template.
type tplRow struct {
	app.TemplateInfo
	Host string
	Err  error
}

type templatesMsg struct{ rows []tplRow }

type templatesDoneMsg struct {
	logIdx int
	title  string
	status logStatus
	rows   []tplRow
}

// fetchTemplates lists every host's templates: through listTemplates when
// set, else app.ListAllTemplates over the resolver. It never returns nil.
func (m model) fetchTemplates() []tplRow {
	var hr []app.HostTemplates
	switch {
	case m.listTemplates != nil:
		hr = m.listTemplates(context.Background())
	case m.resolve != nil:
		hr = m.svc().ListAllTemplates(context.Background(), m.hostNames, m.resolve, app.ListTimeout)
	default:
		hr = []app.HostTemplates{{Host: m.host, Err: fmt.Errorf("host %q: no host resolver", m.host)}}
	}
	rows := []tplRow{}
	for _, h := range hr {
		if h.Err != nil {
			rows = append(rows, tplRow{Host: h.Host, Err: h.Err})
			continue
		}
		for _, t := range h.Templates {
			rows = append(rows, tplRow{TemplateInfo: t, Host: h.Host})
		}
	}
	return rows
}

func (m model) templatesCmd() tea.Cmd {
	return func() tea.Msg { return templatesMsg{rows: m.fetchTemplates()} }
}

func (m model) enterTemplatesMode() (tea.Model, tea.Cmd) {
	m.mode = modeTemplates
	m.tplCursor = 0
	return m, m.templatesCmd()
}

func (m *model) clampTemplateCursor() {
	if m.tplCursor >= len(m.tplRows) {
		m.tplCursor = len(m.tplRows) - 1
	}
	if m.tplCursor < 0 {
		m.tplCursor = 0
	}
}

func (m model) updateTemplates(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode = modeList
	case "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.tplCursor > 0 {
			m.tplCursor--
		}
	case "down", "j":
		if m.tplCursor < len(m.tplRows)-1 {
			m.tplCursor++
		}
	case "r":
		return m, m.templatesCmd()
	case "D":
		if m.tplCursor >= len(m.tplRows) || m.tplRows[m.tplCursor].Err != nil {
			return m, nil
		}
		row := m.tplRows[m.tplCursor]
		name := row.Host + "/" + row.Name
		warn := ""
		if row.Stale {
			warn = " (stale)"
		}
		m.confirmMsg = fmt.Sprintf("Delete template %q%s, %s in pool %s? Its %d clones keep working.", name, warn, row.LV, row.Pool, row.Clones)
		m.confirmBack = modeTemplates
		m.confirmAction = func() (tea.Model, tea.Cmd) {
			m.mode, m.confirmBack = modeTemplates, modeList
			return m.deleteTemplate(row)
		}
		m.mode = modeConfirm
	}
	return m, nil
}

// deleteTemplate removes row's template on its host, by LV name and pool (a
// name can match a current and a stale template at once), then lists again.
func (m model) deleteTemplate(row tplRow) (tea.Model, tea.Cmd) {
	name := row.Host + "/" + row.Name
	idx := m.addLog(fmt.Sprintf("delete template %s...", name))
	svc := m.svc()
	return m, func() tea.Msg {
		err := m.onHost(row.Host, func(mgr app.Manager) error {
			_, err := svc.DeleteTemplate(context.Background(), mgr, row.LV, row.Pool)
			return err
		})
		title, status := fmt.Sprintf("delete template %s (%s)", name, row.LV), logStatusDone
		if err != nil {
			title, status = fmt.Sprintf("delete template %s: error: %v", name, err), logStatusError
		}
		return templatesDoneMsg{logIdx: idx, title: title, status: status, rows: m.fetchTemplates()}
	}
}

// humanBytes is a compact binary size: 512B, 1K, 1.5M, 60G.
func humanBytes(n int64) string {
	if n <= 0 {
		return "0"
	}
	if n < 1<<10 {
		return fmt.Sprintf("%dB", n)
	}
	v, unit := float64(n), ""
	for _, u := range []string{"K", "M", "G", "T"} {
		v /= 1024
		unit = u
		if v < 1024 {
			break
		}
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0") + unit
}

func (m model) viewTemplates() string {
	var b strings.Builder
	b.WriteString(promptStyle.Render("Templates"))
	b.WriteString("\n\n")
	if len(m.tplRows) == 0 {
		b.WriteString("  No templates found.\n")
	} else {
		b.WriteString(statusStyle.Render(fmt.Sprintf("  %-28s %-6s %-14s %-12s %-8s %-8s %-6s %-6s %s", "NAME", "ORIGIN", "SHA256", "POOL", "SIZE", "USED", "CLONES", "STALE", "EXPORT")))
		b.WriteString("\n")
		for i, r := range m.tplRows {
			var line string
			switch {
			case r.Err != nil:
				line = fmt.Sprintf("  %-28s ERROR: %s", r.Host, strings.Join(strings.Fields(r.Err.Error()), " "))
			default:
				sha, stale := "-", "-"
				if r.SHA256 != "" {
					sha = r.SHA256[:min(12, len(r.SHA256))]
				}
				if r.Stale {
					stale = "stale"
				}
				line = fmt.Sprintf("  %-28s %-6s %-14s %-12s %-8s %-8s %-6d %-6s %s", r.Host+"/"+r.Name, r.Origin, sha, r.Pool,
					humanBytes(r.SizeBytes), humanBytes(r.UsedBytes), r.Clones, stale, tuiExportCell(r.TemplateInfo))
			}
			if i == m.tplCursor {
				b.WriteString(selectedStyle.Render("> " + line[2:]))
			} else {
				b.WriteString(line)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("D=delete  r=refresh  esc=back"))
	b.WriteString("\n")
	return b.String()
}

// Save a VM as a project template (W on a shut-off VM in the VM list): a name
// prompt, then app.SaveTemplate on the VM's host. The host refuses a disk that
// is not on a thin pool, and the log shows why. tab toggles replace.

// enterSaveTemplateMode asks for the template's name, or logs why the VM
// cannot be saved when it is not shut off.
func (m model) enterSaveTemplateMode(vm vmRow) (tea.Model, tea.Cmd) {
	ref := m.vmRef(vm)
	if vm.State != vmtool.StateShutoff {
		idx := m.addLog(fmt.Sprintf("save template %s: %s is %s: shut it down first: `vmtool stop %s`", ref, ref, vm.State, ref))
		m.log[idx].status = logStatusError
		return m, nil
	}
	m.saveRef, m.saveName, m.saveReplace = ref, "", false
	m.mode = modeSaveTemplate
	return m, nil
}

func (m model) updateSaveTemplate(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k := msg.String(); k {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.mode = modeList
	case "tab":
		m.saveReplace = !m.saveReplace
	case "backspace":
		if n := len(m.saveName); n > 0 {
			m.saveName = m.saveName[:n-1]
		}
	case "enter":
		if m.saveName == "" {
			return m, nil
		}
		m.mode = modeList
		return m.saveTemplate(m.saveRef, m.saveName, m.saveReplace)
	default:
		if len(k) == 1 {
			m.saveName += k
		}
	}
	return m, nil
}

// saveTemplate saves ref as the template name on ref's host, then refreshes
// the VM rows. The log line names the extra disks left out.
func (m model) saveTemplate(ref app.VMRef, name string, replace bool) (tea.Model, tea.Cmd) {
	idx := m.addLog(fmt.Sprintf("save template %s as %s...", ref, name))
	svc := m.svc()
	return m, func() tea.Msg {
		var saved app.SavedTemplate
		err := m.onHost(ref.Host, func(mgr app.Manager) error {
			var err error
			saved, err = svc.SaveTemplate(context.Background(), mgr, app.SaveTemplateInput{Host: ref.Host, VM: ref.Name, Name: name, Replace: replace})
			return err
		})
		title, status := fmt.Sprintf("save template %s as %s/%s (%s)", ref, ref.Host, name, saved.LV), logStatusDone
		if len(saved.ExtraDisks) > 0 {
			title += fmt.Sprintf("; extra disks (%s) are not part of the template", strings.Join(saved.ExtraDisks, ", "))
		}
		if err != nil {
			title, status = fmt.Sprintf("save template %s as %s: error: %v", ref, name, err), logStatusError
		}
		return cmdDoneMsg{logIdx: idx, title: title, status: status, vms: m.fetchRows()}
	}
}

func (m model) viewSaveTemplate() string {
	var b strings.Builder
	b.WriteString(promptStyle.Render("Save as template"))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "  VM:     %s (shut off; its boot disk is snapshotted, nothing else is touched)\n", m.saveRef)
	fmt.Fprintf(&b, "  Name:   %s_\n", m.saveName)
	mark := " "
	if m.saveReplace {
		mark = "x"
	}
	fmt.Fprintf(&b, "  [%s] replace the saved template of that name (tab)\n\n", mark)
	b.WriteString(helpStyle.Render("enter=save  tab=toggle replace  esc=cancel"))
	b.WriteString("\n")
	return b.String()
}

// tuiExportCell is the EXPORT column: "-" with no export volume, else its
// size, "sha" when the digest is known, and when the daemon removes it.
func tuiExportCell(t app.TemplateInfo) string {
	x := t.Export
	if x == nil {
		return "-"
	}
	sum := "no sha"
	if x.SHA256 != "" {
		sum = "sha"
	}
	exp := ""
	if e := x.Expires(); !e.IsZero() {
		exp = ", expires " + e.UTC().Format("01-02 15:04")
	}
	return fmt.Sprintf("%s, %s%s", humanBytes(x.Bytes), sum, exp)
}
