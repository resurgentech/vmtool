package vmtool

import (
	"strings"
	"testing"
)

func TestDiskXMLCache(t *testing.T) {
	x := diskXML("/p/a.qcow2", "vda", "unsafe")
	if !strings.Contains(x, "<driver name='qemu' type='qcow2' cache='unsafe'/>") {
		t.Errorf("unsafe disk XML:\n%s", x)
	}
	x = diskXML("/p/a.qcow2", "vda", "")
	if strings.Contains(x, "cache") {
		t.Errorf("empty cache must emit no attribute:\n%s", x)
	}
}

func TestDomainXMLDiskCache(t *testing.T) {
	cfg := VMConfig{
		Name: "web", DiskPath: "/p/web.qcow2", DiskCache: "writeback",
		ExtraDisks: []DiskDevice{{Path: "/p/web-2.qcow2", Target: "vdb"}},
	}
	x := domainXML(cfg)
	if n := strings.Count(x, "cache='writeback'"); n != 2 {
		t.Errorf("want vda and the extra disk on writeback, found %d:\n%s", n, x)
	}
	cfg.DiskCache = ""
	if x := domainXML(cfg); strings.Contains(x, "cache=") {
		t.Errorf("no DiskCache, yet XML has a cache attribute:\n%s", x)
	}
}

func TestValidDiskCache(t *testing.T) {
	for _, s := range []string{"", "none", "writeback", "writethrough", "unsafe"} {
		if !ValidDiskCache(s) {
			t.Errorf("%q must be valid", s)
		}
	}
	for _, s := range []string{"directsync", "UNSAFE", "x"} {
		if ValidDiskCache(s) {
			t.Errorf("%q must be invalid", s)
		}
	}
}

// libvirt's GetXMLDesc form: vdb listed before vda must not confuse the read.
const cacheDomXML = `<domain type='kvm'><devices>
<disk type='file' device='disk'>
  <driver name='qemu' type='qcow2' cache='writethrough'/>
  <source file='/p/web-2.qcow2'/>
  <target dev='vdb' bus='virtio'/>
</disk>
<disk type='file' device='disk'>
  <driver name='qemu' type='qcow2' cache='unsafe' discard='unmap'/>
  <source file='/p/web.qcow2'/>
  <target dev='vda' bus='virtio'/>
</disk>
</devices></domain>`

func TestDiskCacheOf(t *testing.T) {
	if got := diskCacheOf(cacheDomXML, "vda"); got != "unsafe" {
		t.Errorf("vda = %q, want unsafe", got)
	}
	if got := diskCacheOf(cacheDomXML, "vdb"); got != "writethrough" {
		t.Errorf("vdb = %q", got)
	}
	if got := diskCacheOf(cacheDomXML, "vdc"); got != "" {
		t.Errorf("absent disk = %q, want empty", got)
	}
	none := strings.ReplaceAll(cacheDomXML, " cache='unsafe'", "")
	if got := diskCacheOf(none, "vda"); got != "" {
		t.Errorf("no attribute = %q, want empty", got)
	}
}

// libvirt's GetXMLDesc really writes attributes with double quotes.
func TestDiskCacheOfDoubleQuotes(t *testing.T) {
	dq := strings.ReplaceAll(cacheDomXML, "'", `"`)
	if got := diskCacheOf(dq, "vda"); got != "unsafe" {
		t.Errorf("vda = %q, want unsafe", got)
	}
	if got := diskCacheOf(dq, "vdb"); got != "writethrough" {
		t.Errorf("vdb = %q, want writethrough", got)
	}
	if got := xmlAttr(dq, "<source", "file"); got != "/p/web-2.qcow2" {
		t.Errorf("source file = %q", got)
	}
}

// AddDisk attaches with vda's mode.
func TestAddDiskXMLTakesVdaMode(t *testing.T) {
	x := diskXML("/p/web-3.qcow2", "vdc", diskCacheOf(cacheDomXML, "vda"))
	if !strings.Contains(x, "cache='unsafe'") {
		t.Errorf("added disk lost vda's mode:\n%s", x)
	}
}

// MigrateDisk redefines with the new path and keeps the driver's cache.
func TestRedefineDiskPathKeepsCache(t *testing.T) {
	out := redefineDiskPath(cacheDomXML, "/p/web.qcow2", "/fast/web.qcow2")
	if !strings.Contains(out, "/fast/web.qcow2") || strings.Contains(out, "'/p/web.qcow2'") {
		t.Errorf("path not replaced:\n%s", out)
	}
	if got := diskCacheOf(out, "vda"); got != "unsafe" {
		t.Errorf("vda mode after migrate = %q, want unsafe", got)
	}
	if got := diskCacheOf(out, "vdb"); got != "writethrough" {
		t.Errorf("vdb mode after migrate = %q", got)
	}
}
