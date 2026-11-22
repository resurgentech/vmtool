package tui

import (
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// tplFake (templates_test.go) as a host that can save: VM "vm1" is shut off,
// any other is running.
func (f *tplFake) Info(name string) (*vmtool.VMInfo, error) {
	st := vmtool.StateRunning
	if name == "vm1" {
		st = vmtool.StateShutoff
	}
	return &vmtool.VMInfo{Name: name, State: st}, nil
}

func (f *tplFake) SaveTemplate(vm, name string, replace bool) (app.SavedTemplate, error) {
	r := ""
	if replace {
		r = " replace"
	}
	f.saved = append(f.saved, vm+" "+name+r)
	return app.SavedTemplate{
		TemplateInfo: app.TemplateInfo{Template: vmtool.Template{Name: name, LV: "vmtpl-000000000009", Pool: "thin-test"}},
		ExtraDisks:   []string{"vdb"},
	}, nil
}

func saveModel(t *testing.T) (model, *tplFake) {
	t.Helper()
	m, hv := templatesModel(t)
	m.vms = []vmRow{
		{Host: "veritech01", VMInfo: vmtool.VMInfo{Name: "vm1", State: vmtool.StateShutoff}},
		{Host: "veritech01", VMInfo: vmtool.VMInfo{Name: "vm2", State: vmtool.StateRunning}},
	}
	return m, hv
}

func typeText(t *testing.T, m model, s string) model {
	t.Helper()
	for _, r := range s {
		next, _ := m.Update(key(string(r)))
		m = next.(model)
	}
	return m
}

// W on a shut-off VM asks for a name; enter saves it on the VM's own host and
// the log says what was left out.
func TestSaveTemplateKeyPromptsAndSaves(t *testing.T) {
	m, hv := saveModel(t)
	next, _ := m.Update(key("W"))
	m = next.(model)
	if m.mode != modeSaveTemplate || !strings.Contains(m.View(), "veritech01/vm1") {
		t.Fatalf("W: mode %v\n%s", m.mode, m.View())
	}
	m = typeText(t, m, "proj2")
	if !strings.Contains(m.View(), "proj2") {
		t.Errorf("the typed name is not shown:\n%s", m.View())
	}
	next, cmd := m.Update(key("enter"))
	m = next.(model)
	if m.mode != modeList || cmd == nil {
		t.Fatalf("enter: mode %v cmd %v", m.mode, cmd)
	}
	msg, ok := cmd().(cmdDoneMsg)
	if !ok || msg.status != logStatusDone {
		t.Fatalf("msg = %+v", msg)
	}
	if strings.Join(hv.saved, "|") != "vm1 proj2" {
		t.Errorf("saved %v", hv.saved)
	}
	if !strings.Contains(msg.title, "proj2") || !strings.Contains(msg.title, "vdb") {
		t.Errorf("title %q does not name the template and the extra disk", msg.title)
	}
}

func TestSaveTemplateKeyReplaceAndCancel(t *testing.T) {
	m, hv := saveModel(t)
	next, _ := m.Update(key("W"))
	m = typeText(t, next.(model), "proj2")
	next, _ = m.Update(key("tab")) // toggles replace
	m = next.(model)
	if !strings.Contains(m.View(), "[x] replace") {
		t.Errorf("replace not shown:\n%s", m.View())
	}
	next, cmd := m.Update(key("enter"))
	if msg, ok := cmd().(cmdDoneMsg); !ok || msg.status != logStatusDone || strings.Join(hv.saved, "|") != "vm1 proj2 replace" {
		t.Errorf("replace: %+v %v", msg, hv.saved)
	}
	_ = next

	// esc saves nothing
	m, hv = saveModel(t)
	next, _ = m.Update(key("W"))
	m = typeText(t, next.(model), "proj2")
	next, cmd = m.Update(key("esc"))
	if next.(model).mode != modeList || cmd != nil || len(hv.saved) != 0 {
		t.Errorf("esc: mode %v cmd %v saved %v", next.(model).mode, cmd, hv.saved)
	}
}

// A VM that is not shut off is refused in the log with the fix, with no prompt
// and no host call; a bad name is refused by the shared workflow.
func TestSaveTemplateKeyRefusals(t *testing.T) {
	m, hv := saveModel(t)
	next, _ := m.Update(key("down"))
	next, cmd := next.(model).Update(key("W"))
	m = next.(model)
	if m.mode != modeList || cmd != nil || len(m.log) == 0 ||
		!strings.Contains(m.log[len(m.log)-1].title, "shut it down first: `vmtool stop veritech01/vm2`") || m.log[len(m.log)-1].status != logStatusError {
		t.Fatalf("running VM: mode %v log %+v", m.mode, m.log)
	}
	if len(hv.saved) != 0 {
		t.Errorf("saved %v", hv.saved)
	}

	m, hv = saveModel(t)
	next, _ = m.Update(key("W"))
	m = typeText(t, next.(model), "a b")
	next, cmd = m.Update(key("enter"))
	if cmd == nil {
		t.Fatal("no command")
	}
	if msg, ok := cmd().(cmdDoneMsg); !ok || msg.status != logStatusError || len(hv.saved) != 0 {
		t.Errorf("bad name: %+v saved %v", msg, hv.saved)
	}
	// an empty name is not submitted at all
	m, hv = saveModel(t)
	next, _ = m.Update(key("W"))
	next, cmd = next.(model).Update(key("enter"))
	if cmd != nil || len(hv.saved) != 0 {
		t.Errorf("empty name: cmd %v saved %v", cmd, hv.saved)
	}
}
