package vmtool

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePoolType(t *testing.T) {
	for _, tc := range []struct{ name, xml, want string }{
		{"dir", `<pool type='dir'><name>vmtool</name></pool>`, "dir"},
		{"logical", `<pool type='logical'><name>nvme</name></pool>`, "logical"},
		{"double quotes", `<pool type="logical"><name>nvme</name></pool>`, "logical"},
		{"garbage", `not xml at all`, ""},
		{"empty", ``, ""},
		{"no type", `<pool><name>x</name></pool>`, ""},
		{"domain is not a pool", `<domain type='kvm'></domain>`, ""},
	} {
		if got := parsePoolType(tc.xml); got != tc.want {
			t.Errorf("%s: parsePoolType = %q, want %q", tc.name, got, tc.want)
		}
	}
}

const mixedDomXML = `<domain type='kvm'><devices>
<disk type='block' device='disk'>
  <driver name='qemu' type='raw' cache='none'/>
  <source dev='/dev/vg-test/vm1'/>
  <target dev='vda' bus='virtio'/>
</disk>
<disk type='file' device='disk'>
  <driver name='qemu' type='qcow2'/>
  <source file='/p/vm1-1.qcow2'/>
  <target dev='vdb' bus='virtio'/>
</disk>
</devices></domain>`

func TestParseDisksFileAndBlock(t *testing.T) {
	want := []parsedDisk{
		{Path: "/dev/vg-test/vm1", Target: "vda", Kind: DiskBackingBlock},
		{Path: "/p/vm1-1.qcow2", Target: "vdb", Kind: DiskBackingFile},
	}
	for name, x := range map[string]string{
		"single quotes": mixedDomXML,
		"double quotes": strings.ReplaceAll(mixedDomXML, "'", `"`),
	} {
		if got := parseDisks(x); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: parseDisks = %+v, want %+v", name, got, want)
		}
	}
}

func TestParseDiskSourcePathFindsBlockVda(t *testing.T) {
	if got := parseDiskSourcePath(mixedDomXML); got != "/dev/vg-test/vm1" {
		t.Errorf("parseDiskSourcePath = %q, want the block vda", got)
	}
}

func TestDiskBackingOf(t *testing.T) {
	if got := diskBackingOf(mixedDomXML, "vda"); got != DiskBackingBlock {
		t.Errorf("vda = %q, want block", got)
	}
	if got := diskBackingOf(mixedDomXML, "vdb"); got != DiskBackingFile {
		t.Errorf("vdb = %q, want file", got)
	}
	if got := diskBackingOf(mixedDomXML, "vdz"); got != "" {
		t.Errorf("absent = %q, want empty", got)
	}
}

// Delete keeps deleting only file disks in L1; a block disk is left alone.
func TestFileDiskPathsSkipsBlock(t *testing.T) {
	if got := fileDiskPaths(mixedDomXML); !reflect.DeepEqual(got, []string{"/p/vm1-1.qcow2"}) {
		t.Errorf("fileDiskPaths = %v", got)
	}
}

// AddDisk counts a block disk's target as used, so it never picks vda again.
func TestUsedTargetsCountsBlock(t *testing.T) {
	used := usedTargets(mixedDomXML)
	next, err := nextVirtioTarget(used)
	if err != nil || next != "vdc" {
		t.Errorf("next target = %q, %v; want vdc (used %v)", next, err, used)
	}
	blockOnly := `<domain><devices><disk type='block' device='disk'><source dev='/dev/vg-test/a'/><target dev='vda' bus='virtio'/></disk></devices></domain>`
	next, _ = nextVirtioTarget(usedTargets(blockOnly))
	if next != "vdb" {
		t.Errorf("block-only VM: next target = %q, want vdb", next)
	}
}

// Resize and migrate stay file-only until L2: a block first disk is refused.
func TestFirstFileDiskPath(t *testing.T) {
	if _, err := firstFileDiskPath(mixedDomXML); err == nil {
		t.Error("block vda must be refused")
	}
	file := strings.Replace(mixedDomXML, "type='block'", "type='file'", 1)
	file = strings.Replace(file, "<source dev='/dev/vg-test/vm1'/>", "<source file='/p/vm1.qcow2'/>", 1)
	if p, err := firstFileDiskPath(file); err != nil || p != "/p/vm1.qcow2" {
		t.Errorf("file vda = %q, %v", p, err)
	}
	if _, err := firstFileDiskPath("<domain/>"); err == nil {
		t.Error("no disk must be an error")
	}
}
