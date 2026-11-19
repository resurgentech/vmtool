package vmtool

import (
	"fmt"

	libvirt "libvirt.org/go/libvirt"
)

// DomainDisks is what a thin-pool disk operation needs to know of a domain:
// the boot disk (the first <disk>), every target in use, whether the domain is
// live (running or paused) and its vCPU count.
type DomainDisks struct {
	BootPath string
	BootKind string // DiskBackingFile or DiskBackingBlock
	Used     []string
	Running  bool
	VCPUs    uint
}

// parseDomainDisks reads the disk part of a domain XML.
func parseDomainDisks(xml string) DomainDisks {
	d := DomainDisks{Used: usedTargets(xml)}
	if disks := parseDisks(xml); len(disks) > 0 {
		d.BootPath, d.BootKind = disks[0].Path, disks[0].Kind
	}
	return d
}

// nextExtraIndex is the N of the next extra disk's volume name <vm>-N: the
// same rule as extraVolName's caller (2 for the first extra disk).
func nextExtraIndex(used []string) int { return len(used) + 1 }

// ExtraLVName is the thin volume name of the next extra disk of vmName, given
// the targets in use: <vm>-N, as on a thick pool without the .qcow2 suffix.
func ExtraLVName(vmName string, used []string) string {
	return fmt.Sprintf("%s-%d", vmName, nextExtraIndex(used))
}

// stateIsLive: a running or paused domain is resized and attached to online.
func stateIsLive(s libvirt.DomainState) bool {
	return s == libvirt.DOMAIN_RUNNING || s == libvirt.DOMAIN_PAUSED
}

// DomainDisks reads the domain's disks and state.
func (m *Manager) DomainDisks(name string) (*DomainDisks, error) {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return nil, fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	xmlDesc, err := dom.GetXMLDesc(0)
	if err != nil {
		return nil, fmt.Errorf("getting domain XML: %w", err)
	}
	d := parseDomainDisks(xmlDesc)
	state, _, err := dom.GetState()
	if err != nil {
		return nil, fmt.Errorf("getting VM state: %w", err)
	}
	d.Running = stateIsLive(state)
	if info, err := dom.GetInfo(); err == nil {
		d.VCPUs = info.NrVirtCpu
	}
	return &d, nil
}

// AttachBlockDisk attaches the existing block device at path as the next free
// virtio disk: live and persistent when the domain is live, persistent only
// when shut off. It creates and removes nothing; the caller owns the device.
func (m *Manager) AttachBlockDisk(name, path string) (*DiskDevice, error) {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return nil, fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	xmlDesc, err := dom.GetXMLDesc(0)
	if err != nil {
		return nil, fmt.Errorf("getting domain XML: %w", err)
	}
	target, err := nextVirtioTarget(usedTargets(xmlDesc))
	if err != nil {
		return nil, err
	}
	var vcpus uint
	if info, err := dom.GetInfo(); err == nil {
		vcpus = info.NrVirtCpu
	}
	flags := libvirt.DOMAIN_DEVICE_MODIFY_CONFIG
	state, _, err := dom.GetState()
	if err != nil {
		return nil, fmt.Errorf("getting VM state: %w", err)
	}
	if stateIsLive(state) {
		flags |= libvirt.DOMAIN_DEVICE_MODIFY_LIVE
	}
	if err := dom.AttachDeviceFlags(attachDiskXML(DiskBackingBlock, path, target, xmlDesc, vcpus), flags); err != nil {
		return nil, fmt.Errorf("attaching disk: %w", err)
	}
	return &DiskDevice{Path: path, Target: target, Kind: DiskBackingBlock}, nil
}

// BlockResizeBoot tells a live domain that its boot disk (vda) is now
// sizeBytes: the step after a thin volume has been extended.
func (m *Manager) BlockResizeBoot(name string, sizeBytes uint64) error {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	if err := dom.BlockResize("vda", sizeBytes, libvirt.DOMAIN_BLOCK_RESIZE_BYTES); err != nil {
		return fmt.Errorf("resizing disk online: %w", err)
	}
	return nil
}
