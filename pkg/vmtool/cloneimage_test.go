package vmtool

import (
	"strconv"
	"testing"
)

const gib = uint64(1) << 30

// The clone XML into a logical pool: volume named <vm>, no format, capacity
// and allocation both max(image, requested) in bytes.
func TestCloneVolumeXML(t *testing.T) {
	logical := func(name string, c uint64) string {
		return "<volume>\n  <name>" + name + "</name>\n  <capacity unit='bytes'>" + u64(c) +
			"</capacity>\n  <allocation unit='bytes'>" + u64(c) + "</allocation>\n</volume>"
	}
	dir := func(name string, c uint64) string {
		return "<volume>\n  <name>" + name + "</name>\n  <capacity>" + u64(c) +
			"</capacity>\n  <target>\n    <format type='qcow2'/>\n  </target>\n</volume>"
	}
	for _, tc := range []struct {
		name, poolType, vm string
		image, want        uint64
		wantName, wantXML  string
	}{
		{"logical grows", "logical", "vm1", 10 * gib, 20 * gib, "vm1", logical("vm1", 20*gib)},
		{"logical keeps image size", "logical", "vm1", 10 * gib, 5 * gib, "vm1", logical("vm1", 10*gib)},
		{"logical no size asked", "logical", "vm1", 10 * gib, 0, "vm1", logical("vm1", 10*gib)},
		{"dir ignores size", "dir", "vm1", 10 * gib, 20 * gib, "vm1.qcow2", dir("vm1.qcow2", 10*gib)},
		{"unknown type is dir", "", "vm1", 10 * gib, 20 * gib, "vm1.qcow2", dir("vm1.qcow2", 10*gib)},
	} {
		n, x := cloneVolumeXML(tc.poolType, tc.vm, tc.image, tc.want)
		if n != tc.wantName || x != tc.wantXML {
			t.Errorf("%s:\n got %q\n%s\nwant %q\n%s", tc.name, n, x, tc.wantName, tc.wantXML)
		}
	}
}

func u64(v uint64) string { return strconv.FormatUint(v, 10) }
