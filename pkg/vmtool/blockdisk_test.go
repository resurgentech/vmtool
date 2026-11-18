package vmtool

import (
	"strings"
	"testing"

	"libvirt.org/go/libvirt"
)

// goldenFileDomain is the full domain XML domainXML wrote for a file-backed
// VM before block disks existed (captured from the code at L1, 48b3dd1).
// It must not change: file-backed VMs keep today's XML byte for byte.
const goldenFileDomain = `<domain type='kvm'>
  <name>web</name>
  <uuid>11111111-2222-4333-8444-555555555555</uuid>
  <sysinfo type='smbios'><system><entry name='serial'>ds=nocloud;h=web;i=11111111-2222-4333-8444-555555555555</entry></system></sysinfo>
  <memory unit='GiB'>4</memory>
  <vcpu>2</vcpu>
  <os firmware='efi'>
    <type arch='x86_64' machine='q35'>hvm</type>
    <firmware><feature enabled='no' name='secure-boot'/><feature enabled='no' name='enrolled-keys'/></firmware>
    <smbios mode='sysinfo'/>
    <boot dev='hd'/>
  </os>
  <features>
    <acpi/>
    <apic/>
  </features>
  <cpu mode='host-passthrough' check='none'/>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' cache='unsafe'/>
      <source file='/p/web.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' cache='unsafe'/>
      <source file='/p/web-2.qcow2'/>
      <target dev='vdb' bus='virtio'/>
    </disk>
    <interface type="network">
      <source network="default"/>
      <model type="virtio"/>
    </interface>
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
</domain>`

const goldenFileAttach = `<disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' cache='unsafe'/>
      <source file='/p/web-3.qcow2'/>
      <target dev='vdc' bus='virtio'/>
    </disk>`

func fileGoldenConfig() VMConfig {
	return VMConfig{
		Name: "web", UUID: "11111111-2222-4333-8444-555555555555",
		VCPUs: 2, Memory: 4, DiskPath: "/p/web.qcow2", DiskCache: "unsafe",
		Network:    NetworkConfig{Type: NetworkNAT, Source: "default"},
		ExtraDisks: []DiskDevice{{Path: "/p/web-2.qcow2", Target: "vdb"}},
	}
}

func TestGoldenFileDomainXML(t *testing.T) {
	if got := domainXML(fileGoldenConfig()); got != goldenFileDomain {
		t.Errorf("file-backed domain XML changed:\n%s", got)
	}
}

func TestGoldenFileAttachXML(t *testing.T) {
	if got := diskXML("/p/web-3.qcow2", "vdc", "unsafe"); got != goldenFileAttach {
		t.Errorf("file attach XML changed:\n%s", got)
	}
}

const blockVdaNone2 = `<disk type='block' device='disk'>
      <driver name='qemu' type='raw' cache='none' io='native' discard='unmap' queues='2' iothread='1'/>
      <source dev='/dev/vg-test/web'/>
      <target dev='vda' bus='virtio'/>
    </disk>`

func TestBlockDiskXMLGolden(t *testing.T) {
	if got := blockDiskXML("/dev/vg-test/web", "vda", "none", 2, true); got != blockVdaNone2 {
		t.Errorf("block disk XML:\n%s", got)
	}
}

func TestBlockDiskXMLIO(t *testing.T) {
	for _, tc := range []struct {
		cache  string
		wantIO bool
	}{
		{"none", true}, {"directsync", true},
		{"unsafe", false}, {"writeback", false}, {"writethrough", false}, {"", false},
	} {
		x := blockDiskXML("/dev/vg-test/a", "vdb", tc.cache, 1, false)
		if has := strings.Contains(x, "io='native'"); has != tc.wantIO {
			t.Errorf("cache %q: io='native' present = %v, want %v:\n%s", tc.cache, has, tc.wantIO, x)
		}
		if !strings.Contains(x, "discard='unmap'") {
			t.Errorf("cache %q: discard='unmap' missing:\n%s", tc.cache, x)
		}
		if has := strings.Contains(x, "cache="); has != (tc.cache != "") {
			t.Errorf("cache %q: cache attribute present = %v:\n%s", tc.cache, has, x)
		}
		if strings.Contains(x, "iothread=") {
			t.Errorf("cache %q: iothread written without iothreads:\n%s", tc.cache, x)
		}
	}
}

func TestBlockDiskXMLEscapesPath(t *testing.T) {
	x := blockDiskXML(`/dev/vg/a'b&c`, "vdb", "none", 1, false)
	if !strings.Contains(x, "<source dev='/dev/vg/a&#39;b&amp;c'/>") {
		t.Errorf("path not escaped:\n%s", x)
	}
}

func TestDiskQueues(t *testing.T) {
	for vcpus, want := range map[uint]int{0: 1, 1: 1, 2: 2, 4: 4, 8: 4, 64: 4} {
		if got := diskQueues(vcpus); got != want {
			t.Errorf("diskQueues(%d) = %d, want %d", vcpus, got, want)
		}
	}
}

func TestDomainXMLBlockVda(t *testing.T) {
	cfg := fileGoldenConfig()
	cfg.DiskPath = "/dev/vg-test/web"
	cfg.DiskKind = DiskBackingBlock
	cfg.DiskCache = "none"
	want := strings.Replace(goldenFileDomain, "  <vcpu>2</vcpu>\n", "  <vcpu>2</vcpu>\n  <iothreads>1</iothreads>\n", 1)
	oldVda := "<disk type='file' device='disk'>\n      <driver name='qemu' type='qcow2' cache='unsafe'/>\n      <source file='/p/web.qcow2'/>\n      <target dev='vda' bus='virtio'/>\n    </disk>"
	if !strings.Contains(want, oldVda) {
		t.Fatal("test setup: vda not found in golden")
	}
	want = strings.Replace(want, oldVda, blockVdaNone2, 1)
	want = strings.Replace(want, "cache='unsafe'", "cache='none'", 1) // the file vdb follows DiskCache
	if got := domainXML(cfg); got != want {
		t.Errorf("block-vda domain XML:\n%s", got)
	}
}

func TestDomainXMLBlockExtraDiskGetsIothreads(t *testing.T) {
	cfg := fileGoldenConfig()
	cfg.ExtraDisks = []DiskDevice{{Path: "/dev/vg-test/web-2", Target: "vdb", Kind: DiskBackingBlock}}
	x := domainXML(cfg)
	if !strings.Contains(x, "<iothreads>1</iothreads>") || !strings.Contains(x, "<source dev='/dev/vg-test/web-2'/>") {
		t.Errorf("block extra disk:\n%s", x)
	}
	if strings.Contains(domainXML(fileGoldenConfig()), "iothreads") {
		t.Error("file-only VM must have no iothreads")
	}
}

func TestDiskKindFromVol(t *testing.T) {
	for _, tc := range []struct {
		name  string
		found bool
		typ   libvirt.StorageVolType
		want  string
	}{
		{"block volume", true, libvirt.STORAGE_VOL_BLOCK, DiskBackingBlock},
		{"file volume", true, libvirt.STORAGE_VOL_FILE, DiskBackingFile},
		{"unknown path stays file", false, libvirt.STORAGE_VOL_BLOCK, DiskBackingFile},
	} {
		if got := diskKindFromVol(tc.found, tc.typ); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

const (
	blockVMDom = `<domain type='kvm'><vcpu placement='static'>8</vcpu><iothreads>1</iothreads><devices>
<disk type='block' device='disk'><driver name='qemu' type='raw' cache='none' io='native' discard='unmap'/><source dev='/dev/vg-test/vm1'/><target dev='vda' bus='virtio'/></disk>
</devices></domain>`
	fileVMDom = `<domain type='kvm'><vcpu placement='static'>8</vcpu><devices>
<disk type='file' device='disk'><driver name='qemu' type='qcow2' cache='unsafe'/><source file='/p/vm1.qcow2'/><target dev='vda' bus='virtio'/></disk>
</devices></domain>`
)

func TestAttachDiskXML(t *testing.T) {
	// block VM, block disk: cache from vda, queues from vCPUs, iothread kept.
	got := attachDiskXML(DiskBackingBlock, "/dev/vg-test/vm1-2", "vdb", blockVMDom, 8)
	want := "<disk type='block' device='disk'>\n      <driver name='qemu' type='raw' cache='none' io='native' discard='unmap' queues='4' iothread='1'/>\n      <source dev='/dev/vg-test/vm1-2'/>\n      <target dev='vdb' bus='virtio'/>\n    </disk>"
	if got != want {
		t.Errorf("block VM + block disk:\n%s", got)
	}
	// file VM, block disk: no iothread (domain has none), cache unsafe -> no io.
	got = attachDiskXML(DiskBackingBlock, "/dev/vg-test/vm1-2", "vdb", fileVMDom, 1)
	want = "<disk type='block' device='disk'>\n      <driver name='qemu' type='raw' cache='unsafe' discard='unmap' queues='1'/>\n      <source dev='/dev/vg-test/vm1-2'/>\n      <target dev='vdb' bus='virtio'/>\n    </disk>"
	if got != want {
		t.Errorf("file VM + block disk:\n%s", got)
	}
	// file VM, file disk: unchanged.
	got = attachDiskXML(DiskBackingFile, "/p/vm1-2.qcow2", "vdb", fileVMDom, 8)
	if want := diskXML("/p/vm1-2.qcow2", "vdb", "unsafe"); got != want {
		t.Errorf("file VM + file disk:\n%s", got)
	}
}

func TestVolumeXMLAndName(t *testing.T) {
	for _, tc := range []struct{ pool, in, want string }{
		{"logical", "x.qcow2", "x"}, {"logical", "x", "x"},
		{"dir", "x", "x.qcow2"}, {"dir", "x.qcow2", "x.qcow2"},
		{"", "x", "x.qcow2"}, {"logical", "", ""},
	} {
		if got := volNameFor(tc.pool, tc.in); got != tc.want {
			t.Errorf("volNameFor(%q, %q) = %q, want %q", tc.pool, tc.in, got, tc.want)
		}
	}
	wantDir := "<volume>\n  <name>x.qcow2</name>\n  <capacity unit='G'>5</capacity>\n  <target>\n    <format type='qcow2'/>\n  </target>\n</volume>"
	if got := volumeXML("dir", "x.qcow2", 5); got != wantDir {
		t.Errorf("dir volume XML:\n%s", got)
	}
	got := volumeXML("logical", "x", 5)
	if strings.Contains(got, "format") || !strings.Contains(got, "<capacity unit='G'>5</capacity>") || !strings.Contains(got, "<allocation unit='G'>5</allocation>") {
		t.Errorf("logical volume XML:\n%s", got)
	}
}

func TestShouldDeleteDisk(t *testing.T) {
	for _, tc := range []struct {
		kind     string
		found    bool
		poolType string
		want     bool
	}{
		{DiskBackingFile, true, "dir", true},
		{DiskBackingFile, false, "", true}, // as today: the delete is attempted, a missing volume is ignored
		{DiskBackingBlock, true, "logical", true},
		{DiskBackingBlock, false, "", false},
		{DiskBackingBlock, false, "logical", false},
		{DiskBackingBlock, true, "disk", false},
		{DiskBackingBlock, true, "iscsi", false},
		{DiskBackingBlock, true, "dir", false},
		{DiskBackingBlock, true, "", false},
	} {
		if got := shouldDeleteDisk(tc.kind, tc.found, tc.poolType); got != tc.want {
			t.Errorf("shouldDeleteDisk(%s, found=%v, %q) = %v, want %v", tc.kind, tc.found, tc.poolType, got, tc.want)
		}
	}
}

func TestResizeDiskPathBlockFailsBeforeAnything(t *testing.T) {
	_, err := resizeDiskPath(blockVMDom)
	if err == nil || !strings.Contains(err.Error(), "growing a block-device disk is not supported") ||
		!strings.Contains(err.Error(), "create the VM with --disk-size") {
		t.Errorf("block vda: err = %v", err)
	}
	p, err := resizeDiskPath(fileVMDom)
	if err != nil || p != "/p/vm1.qcow2" {
		t.Errorf("file vda: %q, %v", p, err)
	}
}

func TestResizeVolumeAction(t *testing.T) {
	const g = uint64(1) << 30
	for _, tc := range []struct {
		name         string
		kind         string
		capacity, to uint64
		wantResize   bool
		wantErr      bool
	}{
		{"file grows", DiskBackingFile, 10 * g, 20 * g, true, false},
		{"file same size still calls resize (as today)", DiskBackingFile, 10 * g, 10 * g, true, false},
		{"block already big enough", DiskBackingBlock, 20 * g, 10 * g, false, false},
		{"block exactly", DiskBackingBlock, 10 * g, 10 * g, false, false},
		{"block too small", DiskBackingBlock, 10 * g, 20 * g, false, true},
	} {
		resize, err := resizeVolumeAction(tc.kind, tc.capacity, tc.to)
		if resize != tc.wantResize || (err != nil) != tc.wantErr {
			t.Errorf("%s: resize=%v err=%v", tc.name, resize, err)
		}
		if err != nil && !strings.Contains(err.Error(), "growing a block-device disk is not supported") {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
}

func TestMigratePrecheck(t *testing.T) {
	const msg = "migrate-disk does not support block-device disks or logical pools"
	if _, err := migratePrecheck(blockVMDom, "dir"); err == nil || !strings.Contains(err.Error(), msg) {
		t.Errorf("block vda: %v", err)
	}
	if _, err := migratePrecheck(fileVMDom, "logical"); err == nil || !strings.Contains(err.Error(), msg) {
		t.Errorf("logical target: %v", err)
	}
	if p, err := migratePrecheck(fileVMDom, "dir"); err != nil || p != "/p/vm1.qcow2" {
		t.Errorf("file to dir: %q, %v", p, err)
	}
}
