package tui

import (
	"testing"
	"time"
)

// The create form offers "Wait for address" (yes selected) and maps no to
// NoWait.
func TestCreateFormWaitForAddress(t *testing.T) {
	mm, _ := initialModel(formFake{}, "jetfire", t.TempDir(), "inv.yml").enterCreateMode()
	m := mm.(model)
	f := m.formFields[fWait]
	if f.label != "Wait for address" || len(f.options) != 2 || f.options[0] != "yes" || f.options[1] != "no" {
		t.Fatalf("field = %+v, want Wait for address [yes no]", f)
	}
	m.formValues[fName] = "web"
	if in, ok, err := m.createInput(); err != nil || !ok || in.NoWait {
		t.Fatalf("default: ok=%v err=%v NoWait=%v, want false", ok, err, in.NoWait)
	}
	m.formValues[fWait] = "no"
	if in, _, _ := m.createInput(); !in.NoWait {
		t.Error("picked no, NoWait is false")
	}
}

// The TUI's service carries the waits config.yml sets.
func TestSvcGetsConfigWaits(t *testing.T) {
	m := initialModel(formFake{}, "jetfire", t.TempDir(), "inv.yml")
	m.ipTimeout, m.sshTimeout = 45*time.Second, 50*time.Second
	s := m.svc()
	if s.IPTimeout != 45*time.Second || s.SSHTimeout != 50*time.Second {
		t.Errorf("IPTimeout %s SSHTimeout %s", s.IPTimeout, s.SSHTimeout)
	}
}
