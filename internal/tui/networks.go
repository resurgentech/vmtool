package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jaredeh/vmtool/internal/app"
)

// The network list (N from the VM list): the libvirt networks of this host
// with the ones vmtool made marked, read through app.Service.ListNetworkMarks
// like `vmtool networks`. Read-only: r reads again, esc goes back. Making and
// deleting networks is `vmtool net`.

type networksMsg struct {
	marks []app.NetworkMark
	err   error
}

// networksCmd reads the list off the update loop.
func (m model) networksCmd() tea.Cmd {
	return func() tea.Msg {
		mgr, release, err := m.local()
		if err != nil {
			return networksMsg{err: err}
		}
		defer release()
		marks, err := m.svc().ListNetworkMarks(context.Background(), mgr)
		return networksMsg{marks: marks, err: err}
	}
}

func (m model) enterNetworksMode() (tea.Model, tea.Cmd) {
	m.mode = modeNetworks
	m.netMarks, m.netErr, m.netLoaded = nil, nil, false
	return m, m.networksCmd()
}

func (m model) updateNetworks(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode = modeList
	case "ctrl+c":
		return m, tea.Quit
	case "r":
		return m, m.networksCmd()
	}
	return m, nil
}

func (m model) viewNetworks() string {
	var b strings.Builder
	b.WriteString(promptStyle.Render(fmt.Sprintf("Networks on %s", m.host)))
	b.WriteString("\n\n")
	switch {
	case m.netErr != nil:
		b.WriteString("  " + strings.Join(strings.Fields(m.netErr.Error()), " ") + "\n")
	case !m.netLoaded:
		b.WriteString("  Reading the host...\n")
	case len(m.netMarks) == 0:
		b.WriteString("  No active networks.\n")
	default:
		for _, n := range m.netMarks {
			if n.Vmtool && n.Member {
				b.WriteString("  " + n.Name + " (vmtool, member)\n")
			} else if n.Vmtool {
				b.WriteString("  " + n.Name + " (vmtool)\n")
			} else {
				b.WriteString("  " + n.Name + "\n")
			}
		}
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("(vmtool) = made by vmtool, (member) = on another host's network; `vmtool net` manages them  r=refresh  esc=back"))
	b.WriteString("\n")
	return b.String()
}
