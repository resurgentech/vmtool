package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// saveHost is a thin-pool host (tmplHost) with a VM whose state is set, and a
// SaveTemplate that records its call.
type saveHost struct {
	tmplHost
	state   vmtool.VMState
	infoErr error
	saved   SavedTemplate
	saveErr error
}

func (f *saveHost) Info(name string) (*vmtool.VMInfo, error) {
	f.calls = append(f.calls, "Info "+name)
	if f.infoErr != nil {
		return nil, f.infoErr
	}
	return &vmtool.VMInfo{Name: name, State: f.state}, nil
}

func (f *saveHost) SaveTemplate(vm, name string, replace bool) (SavedTemplate, error) {
	f.calls = append(f.calls, fmt.Sprintf("SaveTemplate %s %s %v", vm, name, replace))
	return f.saved, f.saveErr
}

// A save of a shut-off VM asks the host once, and moves no image: nothing is
// uploaded and the image cache is not asked.
func TestSaveTemplateCallsTheHostAndSyncsNothing(t *testing.T) {
	svc := tplSvc(t, true)
	m := &saveHost{state: vmtool.StateShutoff, saved: SavedTemplate{
		TemplateInfo: TemplateInfo{Template: vmtool.Template{Name: "proj", LV: "vmtpl-000000000001", Origin: vmtool.TemplateOriginSaved}},
		ExtraDisks:   []string{"vdb"}}}
	got, err := svc.SaveTemplate(context.Background(), m, SaveTemplateInput{Host: "hv1", VM: "vm1", Name: "proj", Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(m.calls, "|") != "Info vm1|SaveTemplate vm1 proj true" || m.uploads != 0 || m.imagePath != 0 {
		t.Errorf("calls %q uploads %d imagePath %d", m.calls, m.uploads, m.imagePath)
	}
	if got.LV != "vmtpl-000000000001" || len(got.ExtraDisks) != 1 {
		t.Errorf("answer %+v", got)
	}
}

func TestSaveTemplateRefusesAVMThatIsNotShutOff(t *testing.T) {
	for _, st := range []vmtool.VMState{vmtool.StateRunning, vmtool.StatePaused, vmtool.StateCrashed, vmtool.StateUnknown} {
		svc := tplSvc(t, false)
		m := &saveHost{state: st}
		_, err := svc.SaveTemplate(context.Background(), m, SaveTemplateInput{Host: "hv1", VM: "vm1", Name: "proj"})
		var ae *Error
		if !AsError(err, &ae) || ae.Kind != KindConflict || !strings.Contains(err.Error(), "shut it down first: `vmtool stop hv1/vm1`") {
			t.Errorf("%s: %v", st, err)
		}
		for _, c := range m.calls {
			if strings.HasPrefix(c, "SaveTemplate") {
				t.Errorf("%s: the host was asked: %q", st, m.calls)
			}
		}
	}
}

func TestSaveTemplateValidatesAndNeedsThinPools(t *testing.T) {
	svc := tplSvc(t, false)
	m := &saveHost{state: vmtool.StateShutoff}
	for _, in := range []SaveTemplateInput{
		{Host: "hv1", VM: "vm1", Name: "../x"},
		{Host: "hv1", VM: "vm1", Name: ""},
		{Host: "hv1", VM: "a b", Name: "proj"},
		{Host: "hv1", VM: "", Name: "proj"},
	} {
		_, err := svc.SaveTemplate(context.Background(), m, in)
		var ae *Error
		if !AsError(err, &ae) || ae.Kind != KindInvalid {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if len(m.calls) != 0 {
		t.Errorf("the host was asked: %q", m.calls)
	}
	// a host without thin pools (a plain Manager)
	_, err := svc.SaveTemplate(context.Background(), &createVMs{}, SaveTemplateInput{Host: "hv1", VM: "vm1", Name: "proj"})
	var ae *Error
	if !AsError(err, &ae) || ae.Kind != KindInvalid || !strings.Contains(err.Error(), "templates are saved from VMs on a thin pool") {
		t.Errorf("plain host: %v", err)
	}
	// a VM that does not exist
	m = &saveHost{infoErr: fmt.Errorf("domain: %w", vmtool.ErrNotFound)}
	_, err = svc.SaveTemplate(context.Background(), m, SaveTemplateInput{Host: "hv1", VM: "vm9", Name: "proj"})
	if !AsError(err, &ae) || ae.Kind != KindNotFound {
		t.Errorf("missing VM: %v", err)
	}
	// the host's answer is passed on, ErrNoThinPools as invalid
	m = &saveHost{state: vmtool.StateShutoff, saveErr: ErrNoThinPools}
	_, err = svc.SaveTemplate(context.Background(), m, SaveTemplateInput{Host: "hv1", VM: "vm1", Name: "proj"})
	if !AsError(err, &ae) || ae.Kind != KindInvalid {
		t.Errorf("no thin pools: %v", err)
	}
}
