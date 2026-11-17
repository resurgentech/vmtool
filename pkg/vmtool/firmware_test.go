package vmtool

import (
	"strings"
	"testing"
)

const smbiosLine = "<smbios mode='sysinfo'/>"

func TestOSXMLEFI(t *testing.T) {
	for _, fw := range []string{FirmwareEFI, ""} { // empty is the default, efi
		x := domainXML(VMConfig{Name: "web", UUID: "u", Firmware: fw})
		for _, want := range []string{
			"<os firmware='efi'>",
			"<firmware><feature enabled='no' name='secure-boot'/><feature enabled='no' name='enrolled-keys'/></firmware>",
			smbiosLine,
		} {
			if !strings.Contains(x, want) {
				t.Errorf("firmware %q: XML missing %q:\n%s", fw, want, x)
			}
		}
		// order: type, firmware, smbios, boot
		ti, fi, si, bi := strings.Index(x, "<type "), strings.Index(x, "<firmware>"), strings.Index(x, "<smbios"), strings.Index(x, "<boot ")
		if !(ti < fi && fi < si && si < bi) {
			t.Errorf("firmware %q: os order wrong: type=%d firmware=%d smbios=%d boot=%d", fw, ti, fi, si, bi)
		}
		if strings.Count(x, "<os") != 1 {
			t.Errorf("want exactly one <os>")
		}
	}
}

func TestOSXMLBIOS(t *testing.T) {
	x := domainXML(VMConfig{Name: "web", UUID: "u", Firmware: FirmwareBIOS})
	if !strings.Contains(x, "<os>\n") || strings.Contains(x, "firmware") {
		t.Errorf("bios must emit a plain <os> with no firmware:\n%s", x)
	}
	if !strings.Contains(x, smbiosLine) {
		t.Errorf("bios XML lost the SMBIOS line:\n%s", x)
	}
}

const domcapsEFI = `<domainCapabilities>
  <path>/usr/bin/qemu-system-x86_64</path>
  <domain>kvm</domain>
  <machine>pc-q35-9.0</machine>
  <arch>x86_64</arch>
  <vcpu max='4096'/>
  <os supported='yes'>
    <enum name='firmware'>
      <value>efi</value>
    </enum>
    <loader supported='yes'>
      <value>/usr/share/edk2/x64/OVMF_CODE.4m.fd</value>
      <enum name='type'>
        <value>rom</value>
        <value>pflash</value>
      </enum>
      <enum name='readonly'>
        <value>yes</value>
        <value>no</value>
      </enum>
    </loader>
  </os>
</domainCapabilities>`

const domcapsNoEFI = `<domainCapabilities>
  <path>/usr/bin/qemu-system-x86_64</path>
  <domain>kvm</domain>
  <machine>pc-q35-9.0</machine>
  <arch>x86_64</arch>
  <os supported='yes'>
    <loader supported='yes'>
      <enum name='type'>
        <value>rom</value>
      </enum>
    </loader>
  </os>
</domainCapabilities>`

const domcapsOSUnsupported = `<domainCapabilities><os supported='no'><enum name='firmware'><value>efi</value></enum></os></domainCapabilities>`

func TestDomCapsOffersEFI(t *testing.T) {
	cases := []struct {
		name, xml string
		want      bool
	}{
		{"with efi", domcapsEFI, true},
		{"without efi", domcapsNoEFI, false},
		{"os unsupported", domcapsOSUnsupported, false},
		{"garbage", "not xml", false},
	}
	for _, c := range cases {
		if got := domCapsOffersEFI(c.xml); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNoUEFIError(t *testing.T) {
	msg := noUEFIError("veritech04").Error()
	for _, want := range []string{
		"UEFI firmware (OVMF) is not installed on veritech04",
		"just prereqs veritech04",
		"--firmware bios",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q lacks %q", msg, want)
		}
	}
}

func TestDomainFirmware(t *testing.T) {
	cases := []struct {
		name, xml, want string
	}{
		{"firmware attr", `<domain><os firmware='efi'><type>hvm</type><loader readonly='yes' type='pflash'>/x/OVMF_CODE.fd</loader><nvram>/v/web_VARS.fd</nvram></os></domain>`, FirmwareEFI},
		{"loader pflash only", `<domain><os><type>hvm</type><loader readonly='yes' type='pflash'>/x/OVMF_CODE.fd</loader><nvram template='/x/VARS.fd'>/v/a.fd</nvram></os></domain>`, FirmwareEFI},
		{"plain bios", `<domain><os><type arch='x86_64' machine='q35'>hvm</type><boot dev='hd'/></os></domain>`, FirmwareBIOS},
		{"explicit bios", `<domain><os firmware='bios'><type>hvm</type></os></domain>`, FirmwareBIOS},
		{"rom loader", `<domain><os><type>hvm</type><loader type='rom'>/x/bios.bin</loader></os></domain>`, FirmwareBIOS},
		{"garbage", `nope`, FirmwareBIOS},
	}
	for _, c := range cases {
		if got := domainFirmware(c.xml); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
