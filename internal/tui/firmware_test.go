package tui

import "testing"

// The create form offers Firmware, efi first and selected, and passes the
// choice on in the create input.
func TestCreateFormFirmware(t *testing.T) {
	mm, _ := initialModel(formFake{}, "jetfire", t.TempDir(), "inv.yml").enterCreateMode()
	m := mm.(model)
	opts := m.formFields[fFirmware].options
	if len(opts) != 2 || opts[0] != "efi" || opts[1] != "bios" {
		t.Fatalf("Firmware options %q, want [efi bios]", opts)
	}
	m.formValues[fName] = "web"
	in, ok, err := m.createInput()
	if err != nil || !ok || in.Firmware != "efi" {
		t.Fatalf("default: ok=%v err=%v firmware=%q, want efi", ok, err, in.Firmware)
	}
	m.formValues[fFirmware] = "bios"
	if in, _, _ := m.createInput(); in.Firmware != "bios" {
		t.Errorf("picked bios, input firmware %q", in.Firmware)
	}
}
