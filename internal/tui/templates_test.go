package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// tplFake is a thin-pool host that records template calls. It removes nothing.
type tplFake struct {
	app.Manager
	deleted []string
	saved   []string
}

func (f *tplFake) ListTemplates() ([]app.TemplateInfo, error) { return nil, nil }
func (f *tplFake) PrepareTemplate(string, string) (app.TemplateInfo, error) {
	return app.TemplateInfo{}, errors.New("not used")
}
func (f *tplFake) DeleteTemplate(name, pool string) (app.TemplateInfo, error) {
	f.deleted = append(f.deleted, name+" "+pool)
	return app.TemplateInfo{Template: vmtool.Template{Name: "img1", LV: name, Pool: pool}}, nil
}

func tplInfo(lv, name, origin string, stale bool, clones int) app.TemplateInfo {
	t := vmtool.Template{Name: name, LV: lv, Pool: "thin-test", Origin: origin, SizeBytes: 10 << 30, UsedBytes: 1 << 30, Clones: clones}
	if origin == vmtool.TemplateOriginImage {
		t.Image, t.SHA256 = name, strings.Repeat(lv[6:8], 32)
	}
	return app.TemplateInfo{Template: t, Stale: stale}
}

func templatesModel(t *testing.T) (model, *tplFake) {
	t.Helper()
	hv := &tplFake{}
	m := initialModel(&tuiFake{host: "jetfire"}, "jetfire", "ansible/playbooks", "ansible/inventory.yml")
	m.hostNames = []string{"jetfire", "veritech01", "veritech02"}
	m.listTemplates = func(context.Context) []app.HostTemplates {
		return []app.HostTemplates{
			{Host: "veritech01", Templates: []app.TemplateInfo{
				tplInfo("vmtpl-abababababab", "img1", "image", false, 2),
				tplInfo("vmtpl-cdcdcdcdcdcd", "img1", "image", true, 0),
				tplInfo("vmtpl-000000000001", "proj", "saved", false, 1),
			}},
			{Host: "veritech02", Err: errors.New("timed out after 5s")},
		}
	}
	m.resolve = func(host string) (app.Manager, func(), error) {
		if host == "veritech01" {
			return hv, func() {}, nil
		}
		return nil, nil, errors.New("unexpected host " + host)
	}
	return m, hv
}

func key(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// T opens the templates view; it lists every host's templates with the stale
// mark, and a host that did not answer as an error row.
func TestTemplatesViewShowsStaleMark(t *testing.T) {
	m, _ := templatesModel(t)
	next, cmd := m.Update(key("T"))
	m = next.(model)
	if m.mode != modeTemplates || cmd == nil {
		t.Fatalf("T: mode %v, cmd %v", m.mode, cmd)
	}
	next, _ = m.Update(cmd())
	m = next.(model)
	out := m.View()
	for _, want := range []string{"veritech01/img1", "veritech01/proj", "abababababab", "cdcdcdcdcdcd", "thin-test", "10G", "ERROR: timed out after 5s", "STALE"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	// only the stale row carries the mark
	var marked []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "stale") && !strings.Contains(l, "STALE") && !strings.Contains(l, "stale=") {
			marked = append(marked, l)
		}
	}
	if len(marked) != 1 || !strings.Contains(marked[0], "cdcdcdcdcdcd") {
		t.Errorf("stale marks on %q, want only the cdcd row", marked)
	}
	// esc is back to the VM list
	next, _ = m.Update(key("esc"))
	if next.(model).mode != modeList {
		t.Errorf("esc left mode %v", next.(model).mode)
	}
}

// D on a template asks first; y deletes it on its own host, by LV name and
// pool (never by a name that could match two); n deletes nothing and stays in
// the view.
func TestTemplatesDeleteConfirmAndCallsThrough(t *testing.T) {
	m, hv := templatesModel(t)
	next, cmd := m.Update(key("T"))
	next, _ = next.(model).Update(cmd())
	m = next.(model)

	// the cursor starts on the first row; move to the stale one
	next, _ = m.Update(key("down"))
	m = next.(model)
	next, _ = m.Update(key("D"))
	m = next.(model)
	if m.mode != modeConfirm || !strings.Contains(m.confirmMsg, "veritech01/img1") || !strings.Contains(m.confirmMsg, "vmtpl-cdcdcdcdcdcd") {
		t.Fatalf("D: mode %v msg %q", m.mode, m.confirmMsg)
	}
	// cancel: nothing deleted, back in the templates view
	next, _ = m.Update(key("n"))
	m2 := next.(model)
	if m2.mode != modeTemplates || len(hv.deleted) != 0 {
		t.Fatalf("n: mode %v deleted %v", m2.mode, hv.deleted)
	}
	// confirm
	next, cmd = m.Update(key("y"))
	m = next.(model)
	if m.mode != modeTemplates || cmd == nil {
		t.Fatalf("y: mode %v cmd %v", m.mode, cmd)
	}
	msg, ok := cmd().(templatesDoneMsg)
	if !ok || msg.status != logStatusDone {
		t.Fatalf("msg = %+v", msg)
	}
	if len(hv.deleted) != 1 || hv.deleted[0] != "vmtpl-cdcdcdcdcdcd thin-test" {
		t.Errorf("deleted %v, want the LV name and pool", hv.deleted)
	}
	if len(msg.rows) == 0 {
		t.Error("the view is not refreshed after a delete")
	}

	// an error row and a host the resolver refuses delete nothing
	m, hv = templatesModel(t)
	next, cmd = m.Update(key("T"))
	next, _ = next.(model).Update(cmd())
	m = next.(model)
	m.tplCursor = len(m.tplRows) - 1 // veritech02's error row
	next, _ = m.Update(key("D"))
	if next.(model).mode == modeConfirm {
		t.Error("D on an error row asks to delete")
	}
	if len(hv.deleted) != 0 {
		t.Errorf("deleted %v", hv.deleted)
	}
}
