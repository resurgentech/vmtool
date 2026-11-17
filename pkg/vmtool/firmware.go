package vmtool

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"
)

// Guest firmware values (VMConfig.Firmware, VMInfo.Firmware).
const (
	// FirmwareEFI is UEFI (OVMF) with secure boot off. The default.
	FirmwareEFI = "efi"
	// FirmwareBIOS is legacy BIOS, for an image installed for BIOS.
	FirmwareBIOS = "bios"
)

// ValidFirmware reports whether s is "", "efi" or "bios".
func ValidFirmware(s string) bool {
	return s == "" || s == FirmwareEFI || s == FirmwareBIOS
}

// UEFIHint is added to a create that timed out waiting for an address on a
// VM defined with efi: the usual cause is a BIOS-installed image.
const UEFIHint = "the VM uses UEFI firmware; an image installed for BIOS does not boot under UEFI — delete it and create again with `--firmware bios`"

// firmwareOrDefault maps the empty value to the default, efi.
func firmwareOrDefault(s string) string {
	if s == "" {
		return FirmwareEFI
	}
	return s
}

// domCapsOffersEFI reports whether libvirt domain-capabilities XML offers
// firmware auto-selection of efi (<os supported='yes'> with
// <enum name='firmware'> listing efi). Unparsable XML offers nothing.
func domCapsOffersEFI(domcaps string) bool {
	var caps struct {
		OS struct {
			Supported string `xml:"supported,attr"`
			Enums     []struct {
				Name   string   `xml:"name,attr"`
				Values []string `xml:"value"`
			} `xml:"enum"`
		} `xml:"os"`
	}
	if err := xml.Unmarshal([]byte(domcaps), &caps); err != nil {
		return false
	}
	if caps.OS.Supported != "yes" {
		return false
	}
	for _, e := range caps.OS.Enums {
		if e.Name != "firmware" {
			continue
		}
		for _, v := range e.Values {
			if strings.TrimSpace(v) == FirmwareEFI {
				return true
			}
		}
	}
	return false
}

// noUEFIError is the refusal to define an efi guest on a host without OVMF.
func noUEFIError(host string) error {
	return fmt.Errorf("UEFI firmware (OVMF) is not installed on %s; run `just prereqs %s`, or create with `--firmware bios` for a BIOS image", host, host)
}

// domainFirmware reads the firmware of a domain from its XML: efi when <os>
// says firmware='efi' or has a pflash <loader>, otherwise bios.
func domainFirmware(domXML string) string {
	var d struct {
		OS struct {
			Firmware string `xml:"firmware,attr"`
			Loader   struct {
				Type string `xml:"type,attr"`
			} `xml:"loader"`
		} `xml:"os"`
	}
	if err := xml.Unmarshal([]byte(domXML), &d); err != nil {
		return FirmwareBIOS
	}
	if d.OS.Firmware == FirmwareEFI || d.OS.Loader.Type == "pflash" {
		return FirmwareEFI
	}
	return FirmwareBIOS
}

// SetHostName names the host this Manager runs on, for errors that tell the
// user what to do on it. Without it the short OS hostname is used.
func (m *Manager) SetHostName(name string) { m.hostName = name }

func (m *Manager) hostLabel() string {
	if m.hostName != "" {
		return m.hostName
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "this host"
	}
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return h
}

// requireUEFI asks libvirt for the q35/x86_64/kvm domain capabilities and
// fails when efi firmware is not offered.
func (m *Manager) requireUEFI() error {
	caps, err := m.conn.GetDomainCapabilities("", "x86_64", "q35", "kvm", 0)
	if err != nil {
		return fmt.Errorf("reading domain capabilities: %w", err)
	}
	if !domCapsOffersEFI(caps) {
		return noUEFIError(m.hostLabel())
	}
	return nil
}
