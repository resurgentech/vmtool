package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// uefiVMs is a createVMs whose WaitForIP times out and whose Define records
// the config it was given.
type uefiVMs struct {
	createVMs
	defined vmtool.VMConfig
}

func (f *uefiVMs) ResizeVolume(string, uint64) error { return nil }
func (f *uefiVMs) Define(c vmtool.VMConfig) error    { f.defined = c; return nil }
func (f *uefiVMs) WaitForIP(string, time.Duration) (string, error) {
	return "", errors.New("timed out waiting for an IP")
}

const uefiHint = "an image installed for BIOS does not boot under UEFI"

func createWith(t *testing.T, firmware string) (*uefiVMs, error, []ProgressEvent) {
	t.Helper()
	t.Chdir(t.TempDir())
	m := &uefiVMs{}
	var evs []ProgressEvent
	// uefiVMs gets no address, so create fails at wait_ip; Define was called.
	_, err := (&Service{}).CreateVM(context.Background(), m, CreateVMInput{
		Host: "jetfire", Name: "web", Image: "u.qcow2", Firmware: firmware,
		OnProgress: func(ev ProgressEvent) { evs = append(evs, ev) },
	})
	return m, err, evs
}

func TestCreateEFITimeoutHint(t *testing.T) {
	for _, fw := range []string{"efi", ""} { // empty is the default, efi
		m, err, evs := createWith(t, fw)
		if err == nil || !strings.Contains(err.Error(), uefiHint) || !strings.Contains(err.Error(), "--firmware bios") {
			t.Errorf("firmware %q: error lacks the UEFI hint: %v", fw, err)
		}
		if m.defined.Firmware != "efi" {
			t.Errorf("firmware %q: Define got %q, want efi", fw, m.defined.Firmware)
		}
		var sawHint bool
		for _, ev := range evs {
			if ev.Stage == "wait_ip" && ev.Status == "error" && strings.Contains(ev.Detail, uefiHint) {
				sawHint = true
			}
		}
		if !sawHint {
			t.Errorf("firmware %q: wait_ip error event lacks the hint: %+v", fw, evs)
		}
	}
}

func TestCreateBIOSTimeoutNoHint(t *testing.T) {
	m, err, evs := createWith(t, "bios")
	if err == nil || strings.Contains(err.Error(), "UEFI") {
		t.Errorf("bios error should not mention UEFI: %v", err)
	}
	if m.defined.Firmware != "bios" {
		t.Errorf("Define got %q, want bios", m.defined.Firmware)
	}
	for _, ev := range evs {
		if strings.Contains(ev.Detail, "UEFI") {
			t.Errorf("bios event mentions UEFI: %+v", ev)
		}
	}
}

func TestCreateInvalidFirmware(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := (&Service{}).CreateVM(context.Background(), &uefiVMs{}, CreateVMInput{Host: "h", Name: "web", Image: "u", Firmware: "coreboot"})
	if ae := asAppErr(t, err); ae.Kind != KindInvalid {
		t.Fatalf("kind %v: %v", ae.Kind, err)
	}
}
