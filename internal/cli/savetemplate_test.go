package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// The fake thin host's save and VM state (tplHost is in templates_test.go).
// shut off unless the VM is named running-vm.
func (h tplHost) Info(name string) (*vmtool.VMInfo, error) {
	h.rec("info " + name)
	st := vmtool.StateShutoff
	if name == "running-vm" {
		st = vmtool.StateRunning
	}
	return &vmtool.VMInfo{Name: name, State: st}, nil
}

func (h tplHost) SaveTemplate(vm, name string, replace bool) (app.SavedTemplate, error) {
	r := ""
	if replace {
		r = " replace"
	}
	h.rec("save " + vm + " " + name + r)
	st := app.SavedTemplate{TemplateInfo: app.TemplateInfo{Template: vmtool.Template{
		Name: name, Pool: "thin-test", LV: "vmtpl-000000000009", Origin: "saved", SizeBytes: 20 << 30}}}
	if vm == "extras" {
		st.ExtraDisks = []string{"vdb", "vdc"}
	}
	return st, h.err
}

const requirementsLine = `Before cloning, check the template requirements: README.md, "What a template must be".`

func TestTemplatesSave(t *testing.T) {
	dir, calls := templatesFixture(t)
	keep := map[string]bool{"vmtool templates save": true}
	r := runGate(t, dir, keep, "templates", "save", "veritech01/vm1", "proj2")
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := strings.Join(*calls, "|"); got != "info vm1|save vm1 proj2" {
		t.Errorf("calls %q", got)
	}
	lines := strings.Split(strings.TrimSpace(r.out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "veritech01/proj2") || !strings.Contains(lines[0], "vmtpl-000000000009") ||
		!strings.Contains(lines[0], "veritech01/vm1") || lines[1] != requirementsLine {
		t.Errorf("output %q", r.out)
	}
	if strings.Contains(r.errOut, "extra disks") {
		t.Errorf("stderr %q", r.errOut)
	}

	// --replace is passed on
	*calls = nil
	if r := runGate(t, dir, keep, "templates", "save", "veritech01/vm1", "proj2", "--replace"); r.err != nil || strings.Join(*calls, "|") != "info vm1|save vm1 proj2 replace" {
		t.Errorf("replace: %v %q", r.err, *calls)
	}

	// extra disks: one warning line, before the requirements line, which stays last
	r = runGate(t, dir, keep, "templates", "save", "veritech01/extras", "proj2")
	if r.err != nil {
		t.Fatal(r.err)
	}
	all := r.out + r.errOut
	if strings.Count(all, "extra disks") != 1 || !strings.Contains(all, "extra disks (vdb, vdc) are not part of the template") {
		t.Errorf("warning: %q / %q", r.out, r.errOut)
	}
	if out := strings.TrimSpace(r.out); !strings.HasSuffix(out, requirementsLine) {
		t.Errorf("the output does not end with the requirements line: %q", r.out)
	}
}

func TestTemplatesSaveRefusals(t *testing.T) {
	dir, calls := templatesFixture(t)
	keep := map[string]bool{"vmtool templates save": true}
	// a running VM names the fix and is never saved; no requirements line
	*calls = nil
	r := runGate(t, dir, keep, "templates", "save", "veritech01/running-vm", "proj2")
	if r.err == nil || !strings.Contains(r.err.Error(), "shut it down first: `vmtool stop veritech01/running-vm`") {
		t.Errorf("running: %v", r.err)
	}
	if strings.Contains(strings.Join(*calls, "|"), "save ") || strings.Contains(r.out, "requirements") {
		t.Errorf("calls %q out %q", *calls, r.out)
	}
	// the host's refusal is the command's error
	*calls = nil
	// argument parsing
	for _, args := range [][]string{
		{"templates", "save"},
		{"templates", "save", "veritech01/vm1"},              // no name
		{"templates", "save", "vm1", "proj2"},                // a bare VM name: host/vm is required
		{"templates", "save", "veritech01/vm1", "../x"},      // a path as the name
		{"templates", "save", "veritech01/vm1", "a b"},       // not a plain name
		{"templates", "save", "veritech01/vm1", "proj", "x"}, // too many
		{"templates", "save", "veritech01/a/b", "proj"},      // not host/vm
	} {
		*calls = nil
		if r := runGate(t, dir, keep, args...); r.err == nil || strings.Contains(strings.Join(*calls, "|"), "save ") {
			t.Errorf("%v: err %v calls %q", args, r.err, *calls)
		}
	}
	if r := runGate(t, dir, keep, "templates", "save", "nosuch/vm1", "proj"); r.err == nil {
		t.Error("unknown host accepted")
	}
	_ = errors.New
}
