package vmtool

import (
	"strings"
	"testing"
)

func TestParseDomainDisks(t *testing.T) {
	xml := `<domain type='kvm'><devices>
<disk type='block' device='disk'><source dev='/dev/vg-test/vm1'/><target dev='vda' bus='virtio'/></disk>
<disk type='block' device='disk'><source dev='/dev/vg-test/vm1-2'/><target dev='vdb' bus='virtio'/></disk>
</devices></domain>`
	d := parseDomainDisks(xml)
	if d.BootPath != "/dev/vg-test/vm1" || d.BootKind != DiskBackingBlock {
		t.Errorf("boot = %q %q", d.BootPath, d.BootKind)
	}
	if strings.Join(d.Used, ",") != "vda,vdb" {
		t.Errorf("used = %v", d.Used)
	}
	if got := nextExtraIndex(d.Used); got != 3 {
		t.Errorf("nextExtraIndex = %d, want 3", got)
	}
	if got := nextExtraIndex(nil); got != 1 {
		t.Errorf("nextExtraIndex(nil) = %d", got)
	}

	file := parseDomainDisks(`<domain><devices><disk type='file' device='disk'><source file='/var/lib/vmtool/images/a.qcow2'/><target dev='vda' bus='virtio'/></disk></devices></domain>`)
	if file.BootKind != DiskBackingFile || file.BootPath != "/var/lib/vmtool/images/a.qcow2" {
		t.Errorf("file boot = %+v", file)
	}
	if empty := parseDomainDisks("<domain/>"); empty.BootPath != "" || len(empty.Used) != 0 {
		t.Errorf("empty = %+v", empty)
	}
}

func TestDomainStateRunning(t *testing.T) {
	// Running and paused domains are resized online; shut-off ones are not.
	if !stateIsLive(1) || !stateIsLive(3) || stateIsLive(5) {
		t.Error("stateIsLive: want running(1) and paused(3) live, shut off(5) not")
	}
}
