package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jaredeh/vmtool/internal/app"
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
		b.WriteString(statusStyle.Render(fmt.Sprintf("  %-28s %-6s %-14s %-12s %-8s %-8s %-6s %s", "NAME", "ORIGIN", "SHA256", "POOL", "SIZE", "USED", "CLONES", "STALE")))
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
				line = fmt.Sprintf("  %-28s %-6s %-14s %-12s %-8s %-8s %-6d %s", r.Host+"/"+r.Name, r.Origin, sha, r.Pool,
					humanBytes(r.SizeBytes), humanBytes(r.UsedBytes), r.Clones, stale)
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
