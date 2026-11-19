package vmtool

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeQcow2Header writes a file that starts like a qcow2 image: the magic
// QFI\xfb, then a big-endian uint64 virtual size at offset 24.
func writeQcow2Header(t *testing.T, virtual uint64) string {
	t.Helper()
	b := make([]byte, 72)
	copy(b, []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint32(b[4:], 3)
	binary.BigEndian.PutUint64(b[24:], virtual)
	p := filepath.Join(t.TempDir(), "img1")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestQcow2VirtualSize(t *testing.T) {
	got, err := Qcow2VirtualSize(writeQcow2Header(t, 10<<30))
	if err != nil || got != 10<<30 {
		t.Fatalf("Qcow2VirtualSize = %d, %v; want %d", got, err, int64(10<<30))
	}
}

func TestQcow2VirtualSizeRefusals(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	raw := make([]byte, 72)
	copy(raw, "not a qcow2 file at all")
	zero := make([]byte, 72)
	copy(zero, []byte{'Q', 'F', 'I', 0xfb})
	huge := make([]byte, 72)
	copy(huge, []byte{'Q', 'F', 'I', 0xfb})
	binary.BigEndian.PutUint64(huge[24:], 1<<63)
	for name, p := range map[string]string{
		"no magic":   write("raw", raw),
		"short":      write("short", []byte{'Q', 'F', 'I', 0xfb, 0, 0}),
		"zero size":  write("zero", zero),
		"size > i64": write("huge", huge),
		"missing":    filepath.Join(dir, "nope"),
	} {
		if n, err := Qcow2VirtualSize(p); err == nil {
			t.Errorf("%s: accepted, size %d", name, n)
		}
	}
}

func TestParseThinDevPath(t *testing.T) {
	for _, tc := range []struct {
		p      string
		vg, lv string
		ok     bool
	}{
		{"/dev/vg-test/vm1", "vg-test", "vm1", true},
		{"/dev/vg-test/vm1-2", "vg-test", "vm1-2", true},
		{"/dev/vg-test/../x", "", "", false},
		{"/dev/vg-test/vm1/", "", "", false},
		{"/dev/vg-test//vm1", "", "", false},
		{"/dev/mapper/vg--test-vm1", "", "", false},
		{"/dev/dm-3", "", "", false},
		{"/dev/vg-test/a/b", "", "", false},
		{"/dev/vg-test/", "", "", false},
		{"/dev/-x/vm1", "", "", false},
		{"/dev/vg-test/-x", "", "", false},
		{"/var/lib/x/y", "", "", false},
		{"dev/vg-test/vm1", "", "", false},
		{"", "", "", false},
	} {
		vg, lv, ok := ParseThinDevPath(tc.p)
		if ok != tc.ok || vg != tc.vg || lv != tc.lv {
			t.Errorf("ParseThinDevPath(%q) = %q, %q, %v; want %q, %q, %v", tc.p, vg, lv, ok, tc.vg, tc.lv, tc.ok)
		}
	}
}

func TestBlockDiskPaths(t *testing.T) {
	want := []string{"/dev/vg-test/vm1"}
	if got := blockDiskPaths(mixedDomXML); !reflect.DeepEqual(got, want) {
		t.Errorf("blockDiskPaths = %q, want %q", got, want)
	}
	if got := blockDiskPaths(`<domain></domain>`); len(got) != 0 {
		t.Errorf("blockDiskPaths of no disks = %q", got)
	}
}

// A thin disk is defined with the explicit block kind: its XML is
// blockDiskXML with cache none, for vda and for an extra disk, and the
// domain gets its iothread.
func TestThinDiskDomainXMLIsBlockDiskXML(t *testing.T) {
	cfg := VMConfig{
		Name: "vm1", UUID: "00000000-0000-4000-8000-000000000001",
		VCPUs: 2, Memory: 4, DiskPath: "/dev/vg-test/vm1", DiskKind: DiskBackingBlock, DiskCache: "none",
		Network:    NetworkConfig{Type: NetworkNAT, Source: "default"},
		ExtraDisks: []DiskDevice{{Path: "/dev/vg-test/vm1-2", Target: "vdb", Kind: DiskBackingBlock}},
	}
	x := domainXML(cfg)
	for _, want := range []string{
		blockDiskXML("/dev/vg-test/vm1", "vda", "none", diskQueues(2), true),
		blockDiskXML("/dev/vg-test/vm1-2", "vdb", "none", diskQueues(2), true),
		"<iothreads>1</iothreads>",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("domain XML lacks %q:\n%s", want, x)
		}
	}
	if strings.Contains(x, "type='file'") {
		t.Errorf("thin disk written as a file disk:\n%s", x)
	}
}
