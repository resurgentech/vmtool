package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jaredeh/vmtool/internal/app"
)

// The inventory view (I from the VM list): the store's record of hosts, VMs
// and machines, read through app.Service.Inventory like `vmtool inventory`.
// Read-only: g adds the VMs their host no longer has, r reads again, esc goes
// back. It is not a live view and says nothing about whether a host is up.

type inventoryMsg struct {
	inv  *app.Inventory
	gone bool
	err  error
}

// inventoryCmd reads the inventory (readInventory when set, else the service)
// off the update loop.
func (m model) inventoryCmd(gone bool) tea.Cmd {
	read := m.readInventory
	if read == nil {
		svc := m.svc()
		read = func(ctx context.Context, host string, gone bool) (*app.Inventory, error) {
			return svc.Inventory(ctx, host, gone)
		}
	}
	return func() tea.Msg {
		inv, err := read(context.Background(), "", gone)
		return inventoryMsg{inv: inv, gone: gone, err: err}
	}
}

func (m model) enterInventoryMode() (tea.Model, tea.Cmd) {
	m.mode = modeInventory
	m.invData, m.invErr, m.invGone = nil, nil, false
	return m, m.inventoryCmd(false)
}

func (m model) updateInventory(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode = modeList
	case "ctrl+c":
		return m, tea.Quit
	case "g":
		return m, m.inventoryCmd(!m.invGone)
	case "r":
		return m, m.inventoryCmd(m.invGone)
	}
	return m, nil
}

func (m model) viewInventory() string {
	var b strings.Builder
	b.WriteString(promptStyle.Render("Inventory (the store's record, not a live view)"))
	b.WriteString("\n\n")
	switch {
	case m.invErr != nil:
		b.WriteString("  " + strings.Join(strings.Fields(m.invErr.Error()), " ") + "\n")
	case m.invData == nil:
		b.WriteString("  Reading the store...\n")
	default:
		var t strings.Builder
		_ = app.WriteInventory(&t, m.invData, m.invGone)
		for _, l := range strings.Split(strings.TrimRight(t.String(), "\n"), "\n") {
			b.WriteString("  " + l + "\n")
		}
	}
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("g=show/hide gone VMs  r=refresh  esc=back"))
	b.WriteString("\n")
	return b.String()
}
