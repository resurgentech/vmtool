package vmtool

import (
	"strings"
	"testing"
)

// vmtool writes nothing of its own into a domain definition (locked decision
// "VM inventory" 4): the domain XML a create defines has no <metadata> at all,
// whatever the config carries.
func TestDomainXMLHasNoMetadata(t *testing.T) {
	cfg := DefaultConfig("vm1", "/var/lib/libvirt/images/vm1.qcow2")
	cfg.UUID = "00000000-0000-4000-8000-000000000001"
	cfg.DiskCache = DiskCacheNone
	cfg.ExtraDisks = []DiskDevice{{Path: "/var/lib/libvirt/images/vm1-2.qcow2", Target: "vdb"}}
	for _, fw := range []string{FirmwareEFI, FirmwareBIOS} {
		cfg.Firmware = fw
		x := domainXML(cfg)
		for _, bad := range []string{"<metadata", "urn:vmtool", "vmtool:vm"} {
			if strings.Contains(x, bad) {
				t.Errorf("%s: domain XML carries %q:\n%s", fw, bad, x)
			}
		}
	}
}

// oldElementDomain is a domain as libvirt hands back one that an earlier
// build of vmtool (N4) defined with its own <metadata> element. None exists
// outside tests; it must still define, list and report without error.
const oldElementDomain = `<domain type='kvm' id='3'>
  <name>vm1</name>
  <uuid>00000000-0000-4000-8000-000000000001</uuid>
  <metadata>
    <vmtool:vm xmlns:vmtool="urn:vmtool:vm:1">
      <origin image="img1" image_sha256="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" clone_kind="thin" pool="nvme-thin" created="2026-11-21T04:05:06Z"/>
      <comment>placeholder comment</comment>
    </vmtool:vm>
  </metadata>
  <memory unit='KiB'>2097152</memory>
  <vcpu placement='static'>2</vcpu>
  <os firmware='efi'>
    <type arch='x86_64' machine='pc-q35-8.2'>hvm</type>
    <boot dev='hd'/>
  </os>
  <devices>
    <disk type='block' device='disk'>
      <driver name='qemu' type='raw' cache='none'/>
      <source dev='/dev/vg-thin/vm1'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/libvirt/images/vm1-2.qcow2'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <interface type='network'>
      <mac address='52:54:00:00:00:01'/>
      <source network='default'/>
      <model type='virtio'/>
    </interface>
  </devices>
</domain>`

// A domain that still carries the old element reads as any other (the XML
// fields Info, List and the report use) and redefines through the one
// edit-and-redefine path; vmtool neither reads nor removes the element.
func TestOldMetadataElementIsIgnored(t *testing.T) {
	if fw := domainFirmware(oldElementDomain); fw != FirmwareEFI {
		t.Errorf("firmware %q", fw)
	}
	if c := diskCacheOf(oldElementDomain, "vda"); c != DiskCacheNone {
		t.Errorf("disk cache %q", c)
	}
	if b := diskBackingOf(oldElementDomain, "vda"); b != DiskBackingBlock {
		t.Errorf("disk backing %q", b)
	}
	macs, nets, err := parseDomainNICs(oldElementDomain)
	if err != nil || len(macs) != 1 || len(nets) != 1 {
		t.Errorf("NICs %v %v err %v", macs, nets, err)
	}
	// the report's disk list (VMDisks) and the thin disk operations
	if disks := parseDisks(oldElementDomain); len(disks) != 2 || disks[0].Path != "/dev/vg-thin/vm1" || disks[1].Target != "vdb" {
		t.Errorf("disks %+v", disks)
	}
	if d := parseDomainDisks(oldElementDomain); d.BootPath != "/dev/vg-thin/vm1" || len(d.Used) != 2 {
		t.Errorf("domain disks %+v", d)
	}
	out := redefineDiskPath(oldElementDomain, "/var/lib/libvirt/images/vm1-2.qcow2", "/var/lib/libvirt/images/p2/vm1-2.qcow2")
	if !strings.Contains(out, "/var/lib/libvirt/images/p2/vm1-2.qcow2") {
		t.Errorf("redefine did not move the disk:\n%s", out)
	}
	if !strings.Contains(out, "urn:vmtool:vm:1") || !strings.Contains(out, "placeholder comment") {
		t.Errorf("redefine removed the old element (vmtool must leave it alone):\n%s", out)
	}
}
