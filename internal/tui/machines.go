package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jaredeh/vmtool/internal/app"
)

// runMachineOp runs fn in the background, logs the result, and re-probes
// machines.
func (m model) runMachineOp(op, name string, fn func(*app.Service) error) (tea.Model, tea.Cmd) {
	return m.runMachineOutput(op+" "+name, func(svc *app.Service) (string, error) {
		return "", fn(svc)
	})
}

// runMachineOutput is runMachineOp for operations with output (playbooks).
func (m model) runMachineOutput(title string, fn func(*app.Service) (string, error)) (tea.Model, tea.Cmd) {
	idx := m.addLog(title + "...")
	svc := m.svc()
	return m, func() tea.Msg {
		out, err := fn(svc)
		msg := cmdDoneMsg{logIdx: idx, title: title + ": done", status: logStatusDone, output: out}
		if err != nil {
			msg.title = fmt.Sprintf("%s: error: %v", title, err)
			msg.status = logStatusError
			var ae *app.Error
			if out == "" && app.AsError(err, &ae) {
				msg.output = ae.Output
			}
		}
		if ms, lerr := svc.ListMachines(context.Background()); lerr == nil {
			msg.machines = ms
		}
		return msg
	}
}

// confirmReboot reboots the selected VM (libvirt ACPI) or machine (ssh).
func (m model) confirmReboot() (tea.Model, tea.Cmd) {
	if mi, ok := m.selectedMachine(); ok {
		name := mi.Name
		rows := append([]app.MachineInfo(nil), m.machines...)
		m.confirmMsg = fmt.Sprintf("Reboot %s %q (%s)?", mi.Kind, name, mi.Host)
		m.confirmAction = func() (tea.Model, tea.Cmd) {
			m.mode = modeList
			idx := m.addLog(fmt.Sprintf("reboot %s...", name))
			svc := m.svc()
			return m, func() tea.Msg {
				info, err := svc.RebootMachine(context.Background(), name)
				if err != nil {
					return cmdDoneMsg{logIdx: idx, title: fmt.Sprintf("reboot %s: error: %v", name, err), status: logStatusError}
				}
				// A probe now would still find it up; show the reboot instead.
				for i := range rows {
					if rows[i].Name == name {
						rows[i] = *info
					}
				}
				return cmdDoneMsg{logIdx: idx, title: "reboot " + name, status: logStatusDone, machines: rows}
			}
		}
		m.mode = modeConfirm
		return m, nil
	}
	vm, ok := m.selectedVM()
	if !ok {
		return m, nil
	}
	name := vm.Name
	m.confirmMsg = fmt.Sprintf("Reboot %q?", name)
	m.confirmAction = func() (tea.Model, tea.Cmd) {
		m.mode = modeList
		return m.runLifecycle("reboot", m.vmRef(vm), func(svc *app.Service, mgr app.Manager) error {
			_, err := svc.RebootVM(context.Background(), mgr, m.vmRef(vm))
			return err
		})
	}
	m.mode = modeConfirm
	return m, nil
}

// --- Add machine mode ---

func (m model) enterAddMachineMode() (tea.Model, tea.Cmd) {
	m.machineFields = []formField{
		{label: "Name", kind: fieldText},
		{label: "Host", kind: fieldText},
		{label: "Kind", kind: fieldSelect, options: []string{"metal", "alien"}},
		{label: "Commission", kind: fieldSelect, options: []string{"false", "true"}, hidden: true},
	}
	m.machineValues = []string{"", "", "metal", "false"}
	m.machineField = 0
	m.mode = modeAddMachine
	return m, nil
}

func (m *model) nextVisibleMachineField(from, dir int) int {
	i := from + dir
	for i >= 0 && i < len(m.machineFields) {
		if !m.machineFields[i].hidden {
			return i
		}
		i += dir
	}
	return from
}

func (m model) updateAddMachine(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	f := m.machineFields[m.machineField]

	switch key {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.mode = modeList
		return m, nil
	case "tab", "down":
		m.machineField = m.nextVisibleMachineField(m.machineField, 1)
	case "shift+tab", "up":
		m.machineField = m.nextVisibleMachineField(m.machineField, -1)
	case "left", "right":
		if f.kind == fieldSelect {
			// Both selects have two options, so either direction toggles.
			if m.machineValues[m.machineField] == f.options[0] {
				m.machineValues[m.machineField] = f.options[1]
			} else {
				m.machineValues[m.machineField] = f.options[0]
			}
			if m.machineField == mfKind {
				// Commission is alien-only.
				m.machineFields[mfCommission].hidden = m.machineValues[mfKind] != "alien"
				if m.machineFields[mfCommission].hidden {
					m.machineValues[mfCommission] = "false"
				}
			}
		}
	case "backspace":
		if f.kind == fieldText {
			v := m.machineValues[m.machineField]
			if len(v) > 0 {
				m.machineValues[m.machineField] = v[:len(v)-1]
			}
		}
	case "enter":
		next := m.nextVisibleMachineField(m.machineField, 1)
		if next != m.machineField {
			m.machineField = next
			return m, nil
		}
		return m.submitAddMachine()
	default:
		if f.kind == fieldText && len(key) == 1 {
			m.machineValues[m.machineField] += key
		}
	}
	return m, nil
}

func (m model) submitAddMachine() (tea.Model, tea.Cmd) {
	in := app.AddMachineInput{
		Name:       strings.TrimSpace(m.machineValues[mfName]),
		Host:       strings.TrimSpace(m.machineValues[mfHost]),
		Kind:       m.machineValues[mfKind],
		Commission: m.machineValues[mfCommission] == "true",
	}
	if in.Name == "" || in.Host == "" {
		return m, nil
	}
	m.mode = modeList
	return m.runMachineOutput("adding "+in.Kind+" "+in.Name, func(svc *app.Service) (string, error) {
		var buf strings.Builder
		in.OnProgress = func(ev app.ProgressEvent) {
			if ev.Output != "" {
				buf.WriteString(ev.Output)
			}
			if ev.Detail != "" && (ev.Status == "done" || ev.Status == "error") {
				buf.WriteString(ev.Detail)
				buf.WriteByte('\n')
			}
		}
		// libvirt only refuses VM names: skipped silently when local libvirt
		// is not used (the own name is listed) or cannot be opened.
		var vms app.Manager
		if !m.ownListed {
			if mgr, release, err := m.local(); err == nil {
				defer release()
				vms = mgr
			}
		}
		_, err := svc.AddMachine(context.Background(), vms, in)
		return buf.String(), err
	})
}

func (m model) viewAddMachine() string {
	var b strings.Builder
	b.WriteString(promptStyle.Render("Add metal/alien machine"))
	b.WriteString("\n\n")

	for i, f := range m.machineFields {
		if f.hidden {
			continue
		}
		cursor := "  "
		if i == m.machineField {
			cursor = "> "
		}
		val := m.machineValues[i]
		if i == m.machineField {
			if f.kind == fieldSelect {
				val = fmt.Sprintf("◀ %s ▶", val)
			} else {
				val += "█"
			}
		}
		b.WriteString(fmt.Sprintf("%s%-20s %s\n", cursor, f.label+":", val))
	}

	b.WriteString("\n")
	b.WriteString(helpStyle.Render("  Needs ssh packer/packer + passwordless sudo (ansible/playbooks/prep_machine.yml)."))
	b.WriteString("\n")
	b.WriteString(helpStyle.Render("tab/↓=next  ↑=prev  ←/→=select  enter=submit  esc=cancel"))
	b.WriteString("\n")
	return b.String()
}
