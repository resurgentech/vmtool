package vmtool

import (
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"libvirt.org/go/libvirt"
)

// Manager wraps a libvirt connection and provides VM lifecycle operations.
type Manager struct {
	conn     *libvirt.Connect
	hostName string // see SetHostName
}

// NewManager opens a connection to the local libvirt daemon and ensures
// the default NAT network exists and is active.
func NewManager() (*Manager, error) {
	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		return nil, fmt.Errorf("connecting to libvirt: %w", err)
	}
	mgr := &Manager{conn: conn}
	if err := mgr.EnsureDefaultNetwork(); err != nil {
		mgr.Close()
		return nil, fmt.Errorf("ensuring default network: %w", err)
	}
	return mgr, nil
}

// Close releases the libvirt connection.
func (m *Manager) Close() error {
	_, err := m.conn.Close()
	return err
}

// diskXML is a single file-backed virtio disk device fragment. An empty
// cache writes no cache attribute (the hypervisor's default).
func diskXML(path, target, cache string) string {
	attr := ""
	if cache != "" {
		attr = fmt.Sprintf(" cache='%s'", xmlEscape(cache))
	}
	return fmt.Sprintf(`<disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'%s/>
      <source file='%s'/>
      <target dev='%s' bus='virtio'/>
    </disk>`, attr, path, target)
}

// blockDiskXML is a single block-backed virtio disk device fragment (a logical
// volume): raw, discard on, multi-queue. io='native' only when the cache mode
// opens the device O_DIRECT (none, directsync); with any other mode libvirt
// refuses it, so the attribute is left out. iothread='1' only when the domain
// has an <iothreads> pool.
func blockDiskXML(path, target, cache string, queues int, iothread bool) string {
	attrs := ""
	if cache != "" {
		attrs += fmt.Sprintf(" cache='%s'", xmlEscape(cache))
	}
	if cache == "none" || cache == "directsync" {
		attrs += " io='native'"
	}
	attrs += fmt.Sprintf(" discard='unmap' queues='%d'", queues)
	if iothread {
		attrs += " iothread='1'"
	}
	return fmt.Sprintf(`<disk type='block' device='disk'>
      <driver name='qemu' type='raw'%s/>
      <source dev='%s'/>
      <target dev='%s' bus='virtio'/>
    </disk>`, attrs, xmlEscape(path), target)
}

// diskQueues is the virtio queue count for a block disk: one per vCPU, at most 4.
func diskQueues(vcpus uint) int {
	if vcpus < 1 {
		return 1
	}
	if vcpus > 4 {
		return 4
	}
	return int(vcpus)
}

// diskKindFromVol decides a disk's kind from what libvirt knows about the
// volume at its path: a block volume is a block disk; a path libvirt does not
// know (found false) or any other volume type stays a file disk, as before.
func diskKindFromVol(found bool, t libvirt.StorageVolType) string {
	if found && t == libvirt.STORAGE_VOL_BLOCK {
		return DiskBackingBlock
	}
	return DiskBackingFile
}

// domainHasIothreads reports whether a domain XML has an <iothreads> element.
func domainHasIothreads(xml string) bool {
	return strings.Contains(xml, "<iothreads>") || strings.Contains(xml, "<iothreads ")
}

// attachDiskXML is the device XML for a disk added to an existing domain:
// the cache mode is vda's, a block disk gets queues from the domain's vCPUs
// and an iothread only if the domain already has iothreads.
func attachDiskXML(kind, path, target, domXML string, vcpus uint) string {
	cache := diskCacheOf(domXML, "vda")
	if kind == DiskBackingBlock {
		return blockDiskXML(path, target, cache, diskQueues(vcpus), domainHasIothreads(domXML))
	}
	return diskXML(path, target, cache)
}

// poolTypeLogical is libvirt's LVM volume group pool type.
const poolTypeLogical = "logical"

// volNameFor is the volume name stored in a pool of poolType: a logical pool
// holds LVs with no .qcow2 suffix, any other pool keeps the suffix.
func volNameFor(poolType, name string) string {
	if poolType == poolTypeLogical {
		return strings.TrimSuffix(name, ".qcow2")
	}
	return normalizeVolName(name)
}

// volumeXML is the XML for a new empty volume of sizeGB. In a logical pool
// there is no <format>, and <allocation> equals <capacity>: a smaller
// allocation makes libvirt create a slow old-style snapshot LV.
func volumeXML(poolType, name string, sizeGB uint) string {
	if poolType == poolTypeLogical {
		return fmt.Sprintf(`<volume>
  <name>%s</name>
  <capacity unit='G'>%d</capacity>
  <allocation unit='G'>%d</allocation>
</volume>`, name, sizeGB, sizeGB)
	}
	return fmt.Sprintf(`<volume>
  <name>%s</name>
  <capacity unit='G'>%d</capacity>
  <target>
    <format type='qcow2'/>
  </target>
</volume>`, name, sizeGB)
}

// shouldDeleteDisk decides whether Delete removes a disk's volume. A file disk
// is always tried (a volume libvirt does not know is ignored). A block disk is
// removed only when libvirt found its volume in a pool of type logical: a
// block path in no pool, or in disk/iscsi/any other pool, is not ours.
func shouldDeleteDisk(kind string, found bool, poolType string) bool {
	if kind != DiskBackingBlock {
		return true
	}
	return found && poolType == poolTypeLogical
}

const errBlockGrow = "growing a block-device disk is not supported: libvirt cannot resize a logical volume; create the VM with --disk-size"

// resizeDiskPath is ResizeDisk's pre-check, run before anything changes: the
// boot disk's path, or an error when it is a block disk (cannot be grown).
func resizeDiskPath(xml string) (string, error) {
	if disks := parseDisks(xml); len(disks) > 0 && disks[0].Kind == DiskBackingBlock {
		return "", fmt.Errorf("%s", errBlockGrow)
	}
	return firstFileDiskPath(xml)
}

// resizeVolumeAction decides ResizeVolume: whether to call vol.Resize. A block
// volume already at least want bytes needs nothing; a smaller one cannot be
// grown. A file volume is always resized, as before.
func resizeVolumeAction(kind string, capacity, want uint64) (bool, error) {
	if kind != DiskBackingBlock {
		return true, nil
	}
	if capacity >= want {
		return false, nil
	}
	return false, fmt.Errorf("%s", errBlockGrow)
}

// migratePrecheck is MigrateDisk's pre-check, run before the VM is stopped:
// the boot disk's path, or an error when vda is a block disk or the target
// pool is logical.
func migratePrecheck(xml, targetPoolType string) (string, error) {
	if disks := parseDisks(xml); (len(disks) > 0 && disks[0].Kind == DiskBackingBlock) || targetPoolType == poolTypeLogical {
		return "", fmt.Errorf("migrate-disk does not support block-device disks or logical pools")
	}
	return firstFileDiskPath(xml)
}

// diskDeviceXML writes one disk of a new domain: block per its kind, file otherwise.
func diskDeviceXML(path, target, kind, cache string, vcpus uint, iothread bool) string {
	if kind == DiskBackingBlock {
		return blockDiskXML(path, target, cache, diskQueues(vcpus), iothread)
	}
	return diskXML(path, target, cache)
}

// anyBlockDisk reports whether the config has a block disk (vda or extra).
func anyBlockDisk(cfg VMConfig) bool {
	if cfg.DiskKind == DiskBackingBlock {
		return true
	}
	for _, d := range cfg.ExtraDisks {
		if d.Path != "" && d.Kind == DiskBackingBlock {
			return true
		}
	}
	return false
}

func disksXML(cfg VMConfig) string {
	iothread := anyBlockDisk(cfg)
	var b strings.Builder
	b.WriteString("    ")
	b.WriteString(diskDeviceXML(cfg.DiskPath, "vda", cfg.DiskKind, cfg.DiskCache, cfg.VCPUs, iothread))
	for i, d := range cfg.ExtraDisks {
		if d.Path == "" {
			continue
		}
		target := d.Target
		if target == "" {
			target = fmt.Sprintf("vd%c", 'b'+i)
		}
		b.WriteString("\n    ")
		b.WriteString(diskDeviceXML(d.Path, target, d.Kind, cfg.DiskCache, cfg.VCPUs, iothread))
	}
	return b.String()
}

// iothreadsXML is the <iothreads> line, only when a disk is a block disk.
func iothreadsXML(cfg VMConfig) string {
	if anyBlockDisk(cfg) {
		return "  <iothreads>1</iothreads>\n"
	}
	return ""
}

// xmlEscape escapes s for use as XML text or inside an attribute value.
func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// hostnameFromName turns a VM name into a hostname: "_" becomes "-" (names
// allowed by ValidMachineName may hold "_", a hostname may not), cut to 63.
func hostnameFromName(name string) string {
	h := strings.ReplaceAll(name, "_", "-")
	if len(h) > 63 {
		h = h[:63]
	}
	return h
}

// newUUID returns a random (version 4) UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// identityXML is the <uuid> and <sysinfo> elements; empty without a UUID.
// The SMBIOS serial is the cloud-init NoCloud datasource hint.
func identityXML(cfg VMConfig) string {
	if cfg.UUID == "" {
		return ""
	}
	return fmt.Sprintf("  <uuid>%s</uuid>\n  <sysinfo type='smbios'><system><entry name='serial'>ds=nocloud;h=%s;i=%s</entry></system></sysinfo>\n",
		xmlEscape(cfg.UUID), xmlEscape(hostnameFromName(cfg.Name)), xmlEscape(cfg.UUID))
}

// osXML is the one place the <os> block is written. Order: <type>, <firmware>
// (efi only), <smbios>, <boot>. With efi libvirt picks OVMF from its firmware
// descriptors and creates the per-VM NVRAM; secure boot is off.
func osXML(cfg VMConfig) string {
	var b strings.Builder
	efi := firmwareOrDefault(cfg.Firmware) == FirmwareEFI
	if efi {
		b.WriteString("  <os firmware='efi'>\n")
	} else {
		b.WriteString("  <os>\n")
	}
	b.WriteString("    <type arch='x86_64' machine='q35'>hvm</type>\n")
	if efi {
		b.WriteString("    <firmware><feature enabled='no' name='secure-boot'/><feature enabled='no' name='enrolled-keys'/></firmware>\n")
	}
	if cfg.UUID != "" {
		b.WriteString("    <smbios mode='sysinfo'/>\n")
	}
	b.WriteString("    <boot dev='hd'/>\n")
	b.WriteString("  </os>\n")
	return b.String()
}

// domainXML builds a full libvirt domain XML from a VMConfig.
func domainXML(cfg VMConfig) string {
	return fmt.Sprintf(`<domain type='kvm'>
  <name>%s</name>
%s  <memory unit='GiB'>%d</memory>
  <vcpu>%d</vcpu>
%s%s  <features>
    <acpi/>
    <apic/>
  </features>
  <cpu mode='host-passthrough' check='none'/>
  <devices>
%s
%s
    <serial type='pty'>
      <target port='0'/>
    </serial>
    <console type='pty'>
      <target type='serial' port='0'/>
    </console>
    <graphics type='vnc' port='-1' autoport='yes'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
    <video>
      <model type='qxl' ram='65536' vram='65536' vgamem='16384' heads='1' primary='yes'/>
    </video>
    <channel type='unix'>
      <target type='virtio' name='org.qemu.guest_agent.0'/>
    </channel>
    <memballoon model='virtio'/>
    <rng model='virtio'>
      <backend model='random'>/dev/urandom</backend>
    </rng>
  </devices>
</domain>`, xmlEscape(cfg.Name), identityXML(cfg), cfg.Memory, cfg.VCPUs, iothreadsXML(cfg), osXML(cfg), disksXML(cfg), networkXML(cfg.Network))
}

// prepareDefine fills a missing UUID and returns the domain XML with the
// config it was built from. It does not touch libvirt.
func prepareDefine(cfg VMConfig) (string, VMConfig) {
	if cfg.UUID == "" {
		u, err := newUUID()
		if err != nil {
			panic(fmt.Sprintf("generating uuid: %v", err)) // crypto/rand failure is unrecoverable
		}
		cfg.UUID = u
	}
	return domainXML(cfg), cfg
}

// Define registers a VM with libvirt without starting it. A config with no
// UUID gets a fresh one, made here, on the host that defines the domain.
func (m *Manager) Define(cfg VMConfig) error {
	if firmwareOrDefault(cfg.Firmware) == FirmwareEFI {
		if err := m.requireUEFI(); err != nil {
			return err
		}
	}
	m.resolveDiskKinds(&cfg)
	domXML, _ := prepareDefine(cfg)
	dom, err := m.conn.DomainDefineXML(domXML)
	if err != nil {
		return fmt.Errorf("defining domain: %w", err)
	}
	if err := dom.Free(); err != nil {
		return err
	}
	return nil
}

// volKindByPath asks libvirt for the volume at path and decides its kind
// (diskKindFromVol); a path libvirt does not know is a file disk.
func (m *Manager) volKindByPath(path string) string {
	vol, err := m.conn.LookupStorageVolByPath(path)
	if err != nil {
		return diskKindFromVol(false, 0)
	}
	defer vol.Free()
	info, err := vol.GetInfo()
	if err != nil {
		return diskKindFromVol(false, 0)
	}
	return diskKindFromVol(true, info.Type)
}

// resolveDiskKinds fills every unset disk kind of cfg from libvirt, on the
// host that defines the domain. A kind already set is kept.
func (m *Manager) resolveDiskKinds(cfg *VMConfig) {
	if cfg.DiskKind == "" {
		cfg.DiskKind = m.volKindByPath(cfg.DiskPath)
	}
	extras := make([]DiskDevice, len(cfg.ExtraDisks))
	copy(extras, cfg.ExtraDisks)
	for i := range extras {
		if extras[i].Kind == "" && extras[i].Path != "" {
			extras[i].Kind = m.volKindByPath(extras[i].Path)
		}
	}
	cfg.ExtraDisks = extras
}

// Start boots a previously defined VM.
func (m *Manager) Start(name string) error {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	return dom.Create()
}

// Stop requests a graceful ACPI shutdown of the VM.
// If the VM is paused, it is resumed first so ACPI shutdown can reach it.
func (m *Manager) Stop(name string) error {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	state, _, err := dom.GetState()
	if err != nil {
		return fmt.Errorf("getting state: %w", err)
	}
	if state == libvirt.DOMAIN_PAUSED {
		if err := dom.Resume(); err != nil {
			return fmt.Errorf("resuming paused domain: %w", err)
		}
	}
	return dom.Shutdown()
}

// Resume unpauses a paused VM.
func (m *Manager) Resume(name string) error {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	return dom.Resume()
}

// Destroy forces an immediate power-off of the VM.
func (m *Manager) Destroy(name string) error {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	return dom.Destroy()
}

// Undefine removes the VM definition from libvirt, and its NVRAM file when it
// has one (a UEFI guest; the flag is harmless for BIOS domains).
func (m *Manager) Undefine(name string) error {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	return dom.UndefineFlags(libvirt.DOMAIN_UNDEFINE_NVRAM)
}

// CloneImage clones basePath into targetPool under newName.qcow2 and returns the new volume path.
func (m *Manager) CloneImage(basePath, newName, targetPool string) (string, error) {
	srcVol, err := m.conn.LookupStorageVolByPath(basePath)
	if err != nil {
		return "", fmt.Errorf("looking up base image at %q: %w", basePath, err)
	}
	defer srcVol.Free()

	srcInfo, err := srcVol.GetInfo()
	if err != nil {
		return "", fmt.Errorf("getting base image info: %w", err)
	}

	if targetPool == "" {
		targetPool = DefaultPool
	}
	pool, err := m.conn.LookupStoragePoolByName(targetPool)
	if err != nil {
		return "", fmt.Errorf("looking up pool %q: %w", targetPool, err)
	}
	defer pool.Free()

	volName := newName + ".qcow2"
	volXML := fmt.Sprintf(`<volume>
  <name>%s</name>
  <capacity>%d</capacity>
  <target>
    <format type='qcow2'/>
  </target>
</volume>`, volName, srcInfo.Capacity)

	newVol, err := pool.StorageVolCreateXMLFrom(volXML, srcVol, 0)
	if err != nil {
		return "", fmt.Errorf("cloning volume: %w", err)
	}
	defer newVol.Free()

	path, err := newVol.GetPath()
	if err != nil {
		return "", fmt.Errorf("getting new volume path: %w", err)
	}
	return path, nil
}

// Create clones the base image, optionally resizes it, defines, and starts a VM.
// cfg.DiskPath should be the path to the base image (from ImagePath).
// It will be replaced with the path to the cloned volume.
func (m *Manager) Create(cfg VMConfig) error {
	if !cfg.Noclone {
		clonedPath, err := m.CloneImage(cfg.DiskPath, cfg.Name, cfg.Pool)
		if err != nil {
			return fmt.Errorf("cloning image: %w", err)
		}
		cfg.DiskPath = clonedPath
	}

	if cfg.DiskSizeGB > 0 {
		if err := m.ResizeVolume(cfg.DiskPath, uint64(cfg.DiskSizeGB)*1024*1024*1024); err != nil {
			return fmt.Errorf("resizing disk: %w", err)
		}
	}

	if cfg.ExtraDiskSizeGB > 0 {
		pool := cfg.ExtraDiskPool
		if pool == "" {
			pool = cfg.Pool
		}
		extra, err := m.createEmptyDisk(cfg.Name, cfg.ExtraDiskSizeGB, pool, []string{"vda"})
		if err != nil {
			return fmt.Errorf("creating extra disk: %w", err)
		}
		cfg.ExtraDisks = append(cfg.ExtraDisks, *extra)
	}

	if err := m.Define(cfg); err != nil {
		return err
	}
	return m.Start(cfg.Name)
}

// ResizeVolume resizes a volume (by path) to the given size in bytes.
func (m *Manager) ResizeVolume(volPath string, sizeBytes uint64) error {
	vol, err := m.conn.LookupStorageVolByPath(volPath)
	if err != nil {
		return fmt.Errorf("looking up volume at %q: %w", volPath, err)
	}
	defer vol.Free()
	info, err := vol.GetInfo()
	if err != nil {
		return fmt.Errorf("getting volume info: %w", err)
	}
	do, err := resizeVolumeAction(diskKindFromVol(true, info.Type), info.Capacity, sizeBytes)
	if err != nil || !do {
		return err
	}
	return vol.Resize(sizeBytes, 0)
}

// Delete stops (force), undefines a VM, and removes its cloned disk volume.
func (m *Manager) Delete(name string, noclone bool) error {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()

	var disks []parsedDisk
	if !noclone {
		if xmlDesc, err := dom.GetXMLDesc(0); err == nil {
			disks = parseDisks(xmlDesc)
		}
	}

	state, _, err := dom.GetState()
	if err != nil {
		return fmt.Errorf("getting state: %w", err)
	}
	if state == libvirt.DOMAIN_RUNNING || state == libvirt.DOMAIN_PAUSED {
		if err := dom.Destroy(); err != nil {
			return fmt.Errorf("destroying domain: %w", err)
		}
	}

	if err := dom.UndefineFlags(libvirt.DOMAIN_UNDEFINE_NVRAM); err != nil {
		return fmt.Errorf("undefining domain: %w", err)
	}

	if noclone || len(disks) == 0 {
		return nil
	}

	for _, d := range disks {
		if err := m.deleteDiskVolume(d); err != nil {
			return err
		}
	}
	return nil
}

// deleteDiskVolume removes one disk's volume if shouldDeleteDisk allows it.
// A file disk is deleted by path as before; a block disk only when libvirt
// finds its volume in a pool of type logical.
func (m *Manager) deleteDiskVolume(d parsedDisk) error {
	if d.Kind != DiskBackingBlock {
		return m.deleteVolumeByPath(d.Path)
	}
	vol, err := m.conn.LookupStorageVolByPath(d.Path)
	if err != nil {
		return nil // in no pool: not ours
	}
	defer vol.Free()
	poolType := ""
	if pool, err := vol.LookupPoolByVolume(); err == nil {
		if x, err := pool.GetXMLDesc(0); err == nil {
			poolType = parsePoolType(x)
		}
		pool.Free()
	}
	if !shouldDeleteDisk(d.Kind, true, poolType) {
		return nil
	}
	if err := vol.Delete(0); err != nil {
		return fmt.Errorf("deleting volume at %q: %w", d.Path, err)
	}
	return nil
}

func domainState(state libvirt.DomainState) VMState {
	switch state {
	case libvirt.DOMAIN_RUNNING:
		return StateRunning
	case libvirt.DOMAIN_SHUTOFF:
		return StateShutoff
	case libvirt.DOMAIN_PAUSED:
		return StatePaused
	case libvirt.DOMAIN_CRASHED:
		return StateCrashed
	default:
		return StateUnknown
	}
}

// Info returns runtime information about a single VM.
func (m *Manager) Info(name string) (*VMInfo, error) {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return nil, fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()

	info, err := dom.GetInfo()
	if err != nil {
		return nil, fmt.Errorf("getting domain info: %w", err)
	}

	state, _, err := dom.GetState()
	if err != nil {
		return nil, fmt.Errorf("getting state: %w", err)
	}

	autostart, _ := dom.GetAutostart()

	vi := &VMInfo{
		Name:      name,
		State:     domainState(state),
		VCPUs:     uint(info.NrVirtCpu),
		Memory:    uint(info.MaxMem / 1024 / 1024),
		Autostart: autostart,
	}

	if state == libvirt.DOMAIN_RUNNING {
		vi.IP, _ = m.getIP(dom)
	}
	if xmlDesc, err := dom.GetXMLDesc(0); err == nil {
		vi.Firmware = domainFirmware(xmlDesc)
		vi.DiskCache = diskCacheOf(xmlDesc, "vda")
		vi.DiskBacking = diskBackingOf(xmlDesc, "vda")
	}

	return vi, nil
}

// List returns info for all VMs managed by libvirt.
func (m *Manager) List() ([]VMInfo, error) {
	domains, err := m.conn.ListAllDomains(libvirt.CONNECT_LIST_DOMAINS_ACTIVE | libvirt.CONNECT_LIST_DOMAINS_INACTIVE)
	if err != nil {
		return nil, fmt.Errorf("listing domains: %w", err)
	}

	var vms []VMInfo
	for _, dom := range domains {
		name, err := dom.GetName()
		if err != nil {
			dom.Free()
			continue
		}
		info, err := dom.GetInfo()
		if err != nil {
			dom.Free()
			continue
		}
		state, _, err := dom.GetState()
		if err != nil {
			dom.Free()
			continue
		}

		autostart, _ := dom.GetAutostart()
		vi := VMInfo{
			Name:      name,
			State:     domainState(state),
			VCPUs:     uint(info.NrVirtCpu),
			Memory:    uint(info.MaxMem / 1024 / 1024),
			Autostart: autostart,
		}
		if state == libvirt.DOMAIN_RUNNING {
			vi.IP, _ = m.getIP(&dom)
		}
		vms = append(vms, vi)
		dom.Free()
	}
	return vms, nil
}

// getIP retrieves the IPv4 address of a running domain by the domain's own
// MAC address(es), read from its XML. Only an interface or lease row whose
// MAC is one of the domain's counts. Order: LEASE (the lease tables of the
// domain's libvirt networks; when a MAC has several rows, as after a
// machine-id change, the newest expiry wins), then the guest AGENT, then
// ARP. An address that the lease table assigns to a different MAC is
// unknown: no address is returned for this domain, whatever the agent or
// ARP report. The choice is made by pickIP.
func (m *Manager) getIP(dom *libvirt.Domain) (string, error) {
	domXML, err := dom.GetXMLDesc(0)
	if err != nil {
		return "", fmt.Errorf("reading domain XML: %w", err)
	}
	macs, networks, err := parseDomainNICs(domXML)
	if err != nil {
		return "", fmt.Errorf("parsing domain XML: %w", err)
	}

	var src IPSources
	for _, name := range networks {
		n, err := m.conn.LookupNetworkByName(name)
		if err != nil {
			continue
		}
		leases, err := n.GetDHCPLeases()
		n.Free()
		if err != nil {
			continue
		}
		for _, l := range leases {
			src.Leases = append(src.Leases, LeaseRow{MAC: l.Mac, IP: l.IPaddr, Expiry: l.ExpiryTime})
		}
	}
	collect := func(s libvirt.DomainInterfaceAddressesSource) []IfaceAddr {
		ifaces, err := dom.ListAllInterfaceAddresses(s)
		if err != nil {
			return nil
		}
		var out []IfaceAddr
		for _, iface := range ifaces {
			ia := IfaceAddr{MAC: iface.Hwaddr}
			for _, a := range iface.Addrs {
				if a.Type == libvirt.IP_ADDR_TYPE_IPV4 {
					ia.IPs = append(ia.IPs, a.Addr)
				}
			}
			out = append(out, ia)
		}
		return out
	}
	src.Agent = collect(libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_AGENT)
	src.ARP = collect(libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_ARP)

	if ip := pickIP(macs, src); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("no IPv4 address found")
}

// WaitForIP polls until the VM has an IP address or the timeout is reached.
func (m *Manager) WaitForIP(name string, timeout time.Duration) (string, error) {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return "", fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ip, err := m.getIP(dom)
		if err == nil && net.ParseIP(ip) != nil {
			return ip, nil
		}
		time.Sleep(time.Second)
	}
	return "", fmt.Errorf("timed out waiting for IP on %q", name)
}

// EnsureDefaultNetwork creates and starts the default NAT network if it doesn't exist.
func (m *Manager) EnsureDefaultNetwork() error {
	net, err := m.conn.LookupNetworkByName("default")
	if err == nil {
		// Network exists, make sure it's active
		active, err := net.IsActive()
		if err != nil {
			net.Free()
			return fmt.Errorf("checking network status: %w", err)
		}
		if !active {
			if err := net.Create(); err != nil {
				net.Free()
				return fmt.Errorf("starting default network: %w", err)
			}
		}
		net.Free()
		return nil
	}

	// Create the default network
	netXML := `<network>
  <name>default</name>
  <forward mode='nat'>
    <nat>
      <port start='1024' end='65535'/>
    </nat>
  </forward>
  <bridge name='virbr0' stp='on' delay='0'/>
  <ip address='192.168.122.1' netmask='255.255.255.0'>
    <dhcp>
      <range start='192.168.122.2' end='192.168.122.254'/>
    </dhcp>
  </ip>
</network>`

	net, err = m.conn.NetworkDefineXML(netXML)
	if err != nil {
		return fmt.Errorf("defining default network: %w", err)
	}
	defer net.Free()

	if err := net.SetAutostart(true); err != nil {
		return fmt.Errorf("setting network autostart: %w", err)
	}
	if err := net.Create(); err != nil {
		return fmt.Errorf("starting default network: %w", err)
	}
	return nil
}

// ListBridges returns host bridge interfaces, excluding libvirt-managed ones (virbr*).
func (m *Manager) ListBridges() ([]string, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil, fmt.Errorf("reading /sys/class/net: %w", err)
	}
	var bridges []string
	for _, e := range entries {
		name := e.Name()
		// A directory with a "bridge" subdirectory is a bridge device
		if _, err := os.Stat(filepath.Join("/sys/class/net", name, "bridge")); err != nil {
			continue
		}
		// Skip libvirt-managed bridges — those should be used via NAT mode
		if strings.HasPrefix(name, "virbr") {
			continue
		}
		bridges = append(bridges, name)
	}
	return bridges, nil
}

// ListNetworks returns the names of available libvirt networks.
func (m *Manager) ListNetworks() ([]string, error) {
	nets, err := m.conn.ListAllNetworks(libvirt.CONNECT_LIST_NETWORKS_ACTIVE)
	if err != nil {
		return nil, fmt.Errorf("listing networks: %w", err)
	}
	var names []string
	for _, n := range nets {
		name, err := n.GetName()
		if err == nil {
			names = append(names, name)
		}
		n.Free()
	}
	return names, nil
}

// GetAutostart returns whether the VM is set to start automatically on host boot.
func (m *Manager) GetAutostart(name string) (bool, error) {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return false, fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	return dom.GetAutostart()
}

// SetAutostart enables or disables automatic start of the VM on host boot.
func (m *Manager) SetAutostart(name string, autostart bool) error {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	return dom.SetAutostart(autostart)
}

// Reboot requests a reboot of the VM.
func (m *Manager) Reboot(name string) error {
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	return dom.Reboot(0)
}

// parsePoolPath extracts the <path> value from a storage pool XML descriptor.
func parsePoolPath(xml string) string {
	si := strings.Index(xml, "<path>")
	if si == -1 {
		return ""
	}
	si += len("<path>")
	ei := strings.Index(xml[si:], "</path>")
	if ei == -1 {
		return ""
	}
	return xml[si : si+ei]
}

// Disk backings: what a domain disk's <source> points at.
const (
	DiskBackingFile  = "file"  // <source file=…>: an image file in a dir pool
	DiskBackingBlock = "block" // <source dev=…>: a block device, e.g. a logical volume
)

// parsedDisk is one <disk> of a domain XML descriptor.
type parsedDisk struct {
	Path   string
	Target string
	Kind   string // DiskBackingFile or DiskBackingBlock
}

// parseDiskSourcePath extracts the first disk's source path (file or block
// device) from a domain XML descriptor.
func parseDiskSourcePath(xml string) string {
	disks := parseDisks(xml)
	if len(disks) == 0 {
		return ""
	}
	return disks[0].Path
}

// firstFileDiskPath is parseDiskSourcePath for the operations that only
// handle file disks (resize, migrate): a block first disk is refused rather
// than touched.
func firstFileDiskPath(xml string) (string, error) {
	disks := parseDisks(xml)
	if len(disks) == 0 || disks[0].Path == "" {
		return "", fmt.Errorf("could not find disk source path in domain XML")
	}
	if disks[0].Kind != DiskBackingFile {
		return "", fmt.Errorf("disk %s is a block device (%s); this operation supports file disks only", disks[0].Target, disks[0].Path)
	}
	return disks[0].Path, nil
}

// parseDisks returns every file- or block-backed <disk> in a domain XML
// descriptor, with its kind.
func parseDisks(xml string) []parsedDisk {
	var out []parsedDisk
	rest := xml
	for {
		start := strings.Index(rest, "<disk")
		if start == -1 {
			break
		}
		rest = rest[start:]
		end := strings.Index(rest, "</disk>")
		if end == -1 {
			break
		}
		block := rest[:end+len("</disk>")]
		rest = rest[end+len("</disk>"):]
		target := xmlAttr(block, "<target", "dev")
		if target == "" {
			continue
		}
		if path := xmlAttr(block, "<source", "file"); path != "" {
			out = append(out, parsedDisk{Path: path, Target: target, Kind: DiskBackingFile})
		} else if path := xmlAttr(block, "<source", "dev"); path != "" {
			out = append(out, parsedDisk{Path: path, Target: target, Kind: DiskBackingBlock})
		}
	}
	return out
}

// fileDiskPaths returns the source path of every file-backed disk.
func fileDiskPaths(xml string) []string {
	var out []string
	for _, d := range parseDisks(xml) {
		if d.Kind == DiskBackingFile {
			out = append(out, d.Path)
		}
	}
	return out
}

// usedTargets returns the target of every disk, file or block.
func usedTargets(xml string) []string {
	var out []string
	for _, d := range parseDisks(xml) {
		out = append(out, d.Target)
	}
	return out
}

// diskBackingOf returns the kind of the disk with target dev, or "" when
// the domain has no such disk.
func diskBackingOf(xml, target string) string {
	for _, d := range parseDisks(xml) {
		if d.Target == target {
			return d.Kind
		}
	}
	return ""
}

// parsePoolType returns the type attribute of the <pool> element in a
// storage pool XML descriptor (dir, logical, …), or "" when there is none.
func parsePoolType(xml string) string {
	return xmlAttr(xml, "<pool", "type")
}

// diskCacheOf returns the cache mode on the <driver> of the disk with target
// dev, or "" when the disk is absent or names none.
func diskCacheOf(domXML, target string) string {
	rest := domXML
	for {
		start := strings.Index(rest, "<disk")
		if start == -1 {
			return ""
		}
		rest = rest[start:]
		end := strings.Index(rest, "</disk>")
		if end == -1 {
			return ""
		}
		block := rest[:end]
		rest = rest[end+len("</disk>"):]
		if xmlAttr(block, "<target", "dev") == target {
			return xmlAttr(block, "<driver", "cache")
		}
	}
}

// xmlAttr returns the value of attr from the first tag that starts with
// tagPrefix. Both quote styles are accepted: vmtool writes attr='…', but
// libvirt's GetXMLDesc returns attr="…".
func xmlAttr(block, tagPrefix, attr string) string {
	ti := strings.Index(block, tagPrefix)
	if ti == -1 {
		return ""
	}
	tag := block[ti:]
	if gt := strings.Index(tag, ">"); gt != -1 {
		tag = tag[:gt]
	}
	for _, q := range []string{"'", `"`} {
		marker := " " + attr + "=" + q
		si := strings.Index(tag, marker)
		if si == -1 {
			continue
		}
		si += len(marker)
		ei := strings.Index(tag[si:], q)
		if ei == -1 {
			return ""
		}
		return tag[si : si+ei]
	}
	return ""
}

func nextVirtioTarget(used []string) (string, error) {
	taken := make(map[string]bool, len(used))
	for _, t := range used {
		taken[t] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		t := "vd" + string(c)
		if !taken[t] {
			return t, nil
		}
	}
	return "", fmt.Errorf("no free virtio disk target (vda–vdz all in use)")
}

func normalizeVolName(name string) string {
	if name == "" {
		return ""
	}
	if !strings.HasSuffix(name, ".qcow2") {
		return name + ".qcow2"
	}
	return name
}

// extraVolName is the conventional volume name for disk N (2, 3, …).
func extraVolName(vmName string, n int) string {
	return fmt.Sprintf("%s-%d.qcow2", vmName, n)
}

func (m *Manager) deleteVolumeByPath(path string) error {
	vol, err := m.conn.LookupStorageVolByPath(path)
	if err != nil {
		return nil // no matching volume, nothing to clean up
	}
	defer vol.Free()
	if err := vol.Delete(0); err != nil {
		return fmt.Errorf("deleting volume at %q: %w", path, err)
	}
	return nil
}

// CreateVolume creates an empty qcow2 volume in pool and returns its path.
// In a logical pool the volume is an LV: the name has no .qcow2 suffix (a
// given x.qcow2 is stored as x) and the volume has no format.
func (m *Manager) CreateVolume(poolName, volName string, sizeGB uint) (string, error) {
	path, _, err := m.createVolume(poolName, volName, sizeGB)
	return path, err
}

// createVolume is CreateVolume that also returns the pool's type.
func (m *Manager) createVolume(poolName, volName string, sizeGB uint) (path, poolType string, err error) {
	if sizeGB == 0 {
		return "", "", fmt.Errorf("size must be greater than 0")
	}
	if poolName == "" {
		poolName = DefaultPool
	}
	if volName == "" {
		return "", "", fmt.Errorf("volume name is required")
	}

	pool, err := m.conn.LookupStoragePoolByName(poolName)
	if err != nil {
		return "", "", fmt.Errorf("looking up pool %q: %w", poolName, err)
	}
	defer pool.Free()

	if x, err := pool.GetXMLDesc(0); err == nil {
		poolType = parsePoolType(x)
	}
	volName = volNameFor(poolType, volName)
	if volName == "" {
		return "", "", fmt.Errorf("volume name is required")
	}

	vol, err := pool.StorageVolCreateXML(volumeXML(poolType, volName, sizeGB), 0)
	if err != nil {
		return "", "", fmt.Errorf("creating volume %q: %w", volName, err)
	}
	defer vol.Free()

	path, err = vol.GetPath()
	if err != nil {
		return "", "", fmt.Errorf("getting volume path: %w", err)
	}
	return path, poolType, nil
}

// kindForPoolType is the kind of a disk whose volume was just made in a pool
// of poolType: an LV in a logical pool is a block disk, anything else a file.
func kindForPoolType(poolType string) string {
	if poolType == poolTypeLogical {
		return DiskBackingBlock
	}
	return DiskBackingFile
}

func (m *Manager) createEmptyDisk(vmName string, sizeGB uint, pool string, usedTargets []string) (*DiskDevice, error) {
	target, err := nextVirtioTarget(usedTargets)
	if err != nil {
		return nil, err
	}
	path, poolType, err := m.createVolume(pool, extraVolName(vmName, len(usedTargets)+1), sizeGB)
	if err != nil {
		return nil, err
	}
	return &DiskDevice{Path: path, Target: target, Kind: kindForPoolType(poolType)}, nil
}

// AddDisk creates an empty qcow2 volume and attaches it as the next virtio disk.
// Volume name is {vm}-N.qcow2 (N is 2 for the first extra disk, then 3, …).
// Live + persistent when the domain is running or paused; persistent only when shut off.
func (m *Manager) AddDisk(name string, sizeGB uint, pool string) (*DiskDevice, error) {
	if sizeGB == 0 {
		return nil, fmt.Errorf("size must be greater than 0")
	}

	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return nil, fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()

	xmlDesc, err := dom.GetXMLDesc(0)
	if err != nil {
		return nil, fmt.Errorf("getting domain XML: %w", err)
	}
	used := usedTargets(xmlDesc)
	var vcpus uint
	if info, err := dom.GetInfo(); err == nil {
		vcpus = info.NrVirtCpu
	}

	disk, err := m.createEmptyDisk(name, sizeGB, pool, used)
	if err != nil {
		return nil, err
	}

	flags := libvirt.DOMAIN_DEVICE_MODIFY_CONFIG
	state, _, err := dom.GetState()
	if err != nil {
		_ = m.deleteVolumeByPath(disk.Path)
		return nil, fmt.Errorf("getting VM state: %w", err)
	}
	if state == libvirt.DOMAIN_RUNNING || state == libvirt.DOMAIN_PAUSED {
		flags |= libvirt.DOMAIN_DEVICE_MODIFY_LIVE
	}

	if err := dom.AttachDeviceFlags(attachDiskXML(disk.Kind, disk.Path, disk.Target, xmlDesc, vcpus), flags); err != nil {
		_ = m.deleteVolumeByPath(disk.Path)
		return nil, fmt.Errorf("attaching disk: %w", err)
	}
	return disk, nil
}

// DefaultPoolPath returns the filesystem path of the DefaultPool storage pool.
func (m *Manager) DefaultPoolPath() (string, error) {
	pool, err := m.conn.LookupStoragePoolByName(DefaultPool)
	if err != nil {
		return "", fmt.Errorf("looking up default pool: %w", err)
	}
	defer pool.Free()

	xml, err := pool.GetXMLDesc(0)
	if err != nil {
		return "", fmt.Errorf("getting pool XML: %w", err)
	}

	path := parsePoolPath(xml)
	if path == "" {
		return "", fmt.Errorf("could not parse path from pool XML")
	}
	return path, nil
}

// ListImages returns the names of .qcow2 volumes across all storage pools.
func (m *Manager) ListImages() ([]string, error) {
	pools, err := m.conn.ListAllStoragePools(0)
	if err != nil {
		return nil, fmt.Errorf("listing storage pools: %w", err)
	}
	var images []string
	for _, pool := range pools {
		vols, err := pool.ListAllStorageVolumes(0)
		if err != nil {
			pool.Free()
			continue
		}
		for _, vol := range vols {
			name, err := vol.GetName()
			vol.Free()
			if err != nil {
				continue
			}
			if strings.HasSuffix(name, ".qcow2") {
				images = append(images, name)
			}
		}
		pool.Free()
	}
	return images, nil
}

// ListImagesByPool returns a map of pool name → .qcow2 image names.
func (m *Manager) ListImagesByPool() (map[string][]string, error) {
	pools, err := m.conn.ListAllStoragePools(0)
	if err != nil {
		return nil, fmt.Errorf("listing storage pools: %w", err)
	}
	result := make(map[string][]string)
	for _, pool := range pools {
		poolName, err := pool.GetName()
		if err != nil {
			pool.Free()
			continue
		}
		vols, err := pool.ListAllStorageVolumes(0)
		if err != nil {
			pool.Free()
			continue
		}
		for _, vol := range vols {
			name, err := vol.GetName()
			vol.Free()
			if err != nil {
				continue
			}
			if strings.HasSuffix(name, ".qcow2") {
				result[poolName] = append(result[poolName], name)
			}
		}
		pool.Free()
	}
	return result, nil
}

// DeleteImage deletes a .qcow2 volume from the named storage pool; an empty
// pool is DefaultPool.
func (m *Manager) DeleteImage(name, pool string) error {
	if pool == "" {
		pool = DefaultPool
	}
	p, err := m.conn.LookupStoragePoolByName(pool)
	if err != nil {
		return fmt.Errorf("looking up pool %q: %w", pool, err)
	}
	defer p.Free()

	vol, err := p.LookupStorageVolByName(name)
	if err != nil {
		return fmt.Errorf("image %q not found in pool %q: %w", name, pool, err)
	}
	defer vol.Free()

	if err := vol.Delete(0); err != nil {
		return fmt.Errorf("deleting image %q: %w", name, err)
	}
	return nil
}

// ImagePath searches all storage pools for a volume by name and returns its path.
func (m *Manager) ImagePath(name string) (string, error) {
	pools, err := m.conn.ListAllStoragePools(0)
	if err != nil {
		return "", fmt.Errorf("listing storage pools: %w", err)
	}
	for _, pool := range pools {
		vol, err := pool.LookupStorageVolByName(name)
		if err != nil {
			pool.Free()
			continue
		}
		path, pathErr := vol.GetPath()
		vol.Free()
		pool.Free()
		if pathErr != nil {
			return "", fmt.Errorf("getting volume path: %w", pathErr)
		}
		return path, nil
	}
	return "", fmt.Errorf("image %q not found in any storage pool: %w", name, ErrNotFound)
}

// FindImage looks an image up with look; when no pool has it, it calls
// refresh once and looks again, so a file dropped into a pool behind
// libvirt's back is seen before "not found" is reported. A failed refresh
// leaves the not-found answer; any other lookup error is returned as is.
func FindImage(name string, look func(string) (string, error), refresh func() error) (string, error) {
	p, err := look(name)
	if err == nil || !IsNotFound(err) || refresh == nil {
		return p, err
	}
	if rerr := refresh(); rerr != nil {
		return "", err
	}
	return look(name)
}

// RefreshPools rescans every active storage pool. Only the workstation calls
// it (to find its own images): a hypervisor daemon never refreshes pools it
// does not own.
func (m *Manager) RefreshPools() error {
	pools, err := m.conn.ListAllStoragePools(0)
	if err != nil {
		return fmt.Errorf("listing storage pools: %w", err)
	}
	var first error
	for _, pool := range pools {
		if active, _ := pool.IsActive(); active {
			if err := pool.Refresh(0); err != nil && first == nil {
				name, _ := pool.GetName()
				first = fmt.Errorf("refreshing pool %q: %w", name, err)
			}
		}
		pool.Free()
	}
	return first
}

// PoolInfo holds metadata about a storage pool.
type PoolInfo struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Active bool   `json:"active"`
	// Type is libvirt's pool type (dir, logical, …); empty when unknown.
	Type string `json:"type,omitempty"`
}

// ListPools returns metadata for all storage pools.
func (m *Manager) ListPools() ([]PoolInfo, error) {
	pools, err := m.conn.ListAllStoragePools(0)
	if err != nil {
		return nil, fmt.Errorf("listing storage pools: %w", err)
	}
	var result []PoolInfo
	for _, pool := range pools {
		name, _ := pool.GetName()
		active, _ := pool.IsActive()
		path, typ := "", ""
		if xmlDesc, err := pool.GetXMLDesc(0); err == nil {
			path = parsePoolPath(xmlDesc)
			typ = parsePoolType(xmlDesc)
		}
		result = append(result, PoolInfo{Name: name, Path: path, Active: active, Type: typ})
		pool.Free()
	}
	return result, nil
}

// CreatePool defines a new directory-type storage pool at path, starts it, and sets autostart.
func (m *Manager) CreatePool(name, path string) error {
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("creating pool directory: %w", err)
	}
	poolXML := fmt.Sprintf(`<pool type='dir'>
  <name>%s</name>
  <target>
    <path>%s</path>
  </target>
</pool>`, name, path)

	pool, err := m.conn.StoragePoolDefineXML(poolXML, 0)
	if err != nil {
		return fmt.Errorf("defining pool: %w", err)
	}
	defer pool.Free()

	if err := pool.Create(0); err != nil {
		return fmt.Errorf("starting pool: %w", err)
	}
	if err := pool.SetAutostart(true); err != nil {
		return fmt.Errorf("setting pool autostart: %w", err)
	}
	return nil
}

// ResizeDisk grows a VM's disk volume to sizeGB gigabytes.
// The new size must be larger than the current capacity.
// Running/paused VMs are resized online via block resize (qemu holds the image
// lock, so offline vol-resize cannot be used). Shut-off VMs use vol resize.
func (m *Manager) ResizeDisk(name string, sizeGB uint) error {
	if sizeGB == 0 {
		return fmt.Errorf("size must be greater than 0")
	}

	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()

	xmlDesc, err := dom.GetXMLDesc(0)
	if err != nil {
		return fmt.Errorf("getting domain XML: %w", err)
	}
	// Before anything changes: a block disk cannot be grown (libvirt does not
	// resize logical volumes), so the VM is not touched.
	diskPath, err := resizeDiskPath(xmlDesc)
	if err != nil {
		return err
	}

	vol, err := m.conn.LookupStorageVolByPath(diskPath)
	if err != nil {
		return fmt.Errorf("looking up volume at %q: %w", diskPath, err)
	}
	defer vol.Free()

	info, err := vol.GetInfo()
	if err != nil {
		return fmt.Errorf("getting volume info: %w", err)
	}

	sizeBytes := uint64(sizeGB) * 1024 * 1024 * 1024
	if sizeBytes <= info.Capacity {
		curGB := info.Capacity / (1024 * 1024 * 1024)
		return fmt.Errorf("new size %dGB must be larger than current %dGB", sizeGB, curGB)
	}

	state, _, err := dom.GetState()
	if err != nil {
		return fmt.Errorf("getting VM state: %w", err)
	}

	// Online: qemu holds the image write lock, so vol.Resize (qemu-img) fails.
	// BlockResize grows the disk through the running domain instead.
	if state == libvirt.DOMAIN_RUNNING || state == libvirt.DOMAIN_PAUSED {
		if err := dom.BlockResize("vda", sizeBytes, libvirt.DOMAIN_BLOCK_RESIZE_BYTES); err != nil {
			return fmt.Errorf("resizing disk online: %w", err)
		}
		return nil
	}

	if err := vol.Resize(sizeBytes, 0); err != nil {
		return fmt.Errorf("resizing volume: %w", err)
	}
	return nil
}

// redefineDiskPath swaps the boot disk's source path in domain XML and
// touches nothing else, so the driver's cache mode carries over.
func redefineDiskPath(xmlDesc, oldPath, newPath string) string {
	return strings.Replace(xmlDesc, oldPath, newPath, 1)
}

// MigrateDisk copies a VM's disk to targetPool, redefines the domain with the new path,
// and restarts the VM if it was running. The VM is force-stopped during the copy.
//
// It redefines in place from GetXMLDesc, so firmware and the NVRAM path carry
// over. It must never undefine first: that would delete the NVRAM file
// (UNDEFINE_NVRAM) and a UEFI guest would lose its variables.
func (m *Manager) MigrateDisk(vmName, targetPool string) error {
	dom, err := m.conn.LookupDomainByName(vmName)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", vmName, err)
	}
	defer dom.Free()

	state, _, err := dom.GetState()
	if err != nil {
		return fmt.Errorf("getting VM state: %w", err)
	}
	wasRunning := state == libvirt.DOMAIN_RUNNING || state == libvirt.DOMAIN_PAUSED

	xmlDesc, err := dom.GetXMLDesc(0)
	if err != nil {
		return fmt.Errorf("getting domain XML: %w", err)
	}
	pool, err := m.conn.LookupStoragePoolByName(targetPool)
	if err != nil {
		return fmt.Errorf("looking up pool %q: %w", targetPool, err)
	}
	defer pool.Free()
	targetType := ""
	if x, err := pool.GetXMLDesc(0); err == nil {
		targetType = parsePoolType(x)
	}
	// Before the VM is stopped: refuse block disks and logical pools.
	oldPath, err := migratePrecheck(xmlDesc, targetType)
	if err != nil {
		return err
	}

	if wasRunning {
		if err := dom.Destroy(); err != nil {
			return fmt.Errorf("stopping VM: %w", err)
		}
	}

	oldVol, err := m.conn.LookupStorageVolByPath(oldPath)
	if err != nil {
		return fmt.Errorf("looking up volume at %q: %w", oldPath, err)
	}
	defer oldVol.Free()

	volInfo, err := oldVol.GetInfo()
	if err != nil {
		return fmt.Errorf("getting volume info: %w", err)
	}
	volName, err := oldVol.GetName()
	if err != nil {
		return fmt.Errorf("getting volume name: %w", err)
	}

	volXML := fmt.Sprintf(`<volume>
  <name>%s</name>
  <capacity>%d</capacity>
  <target>
    <format type='qcow2'/>
  </target>
</volume>`, volName, volInfo.Capacity)

	newVol, err := pool.StorageVolCreateXMLFrom(volXML, oldVol, 0)
	if err != nil {
		return fmt.Errorf("copying volume to pool %q: %w", targetPool, err)
	}
	defer newVol.Free()

	newPath, err := newVol.GetPath()
	if err != nil {
		return fmt.Errorf("getting new volume path: %w", err)
	}

	newXML := redefineDiskPath(xmlDesc, oldPath, newPath)
	if _, err := m.conn.DomainDefineXML(newXML); err != nil {
		return fmt.Errorf("redefining domain: %w", err)
	}

	if err := oldVol.Delete(0); err != nil {
		return fmt.Errorf("deleting old volume: %w", err)
	}

	if wasRunning {
		dom2, err := m.conn.LookupDomainByName(vmName)
		if err != nil {
			return fmt.Errorf("looking up domain after redefine: %w", err)
		}
		defer dom2.Free()
		if err := dom2.Create(); err != nil {
			return fmt.Errorf("restarting VM: %w", err)
		}
	}

	return nil
}
