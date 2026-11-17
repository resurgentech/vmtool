package vmtool

import (
	"regexp"
	"strings"
	"testing"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDomainXMLIdentity(t *testing.T) {
	cfg := VMConfig{Name: "web", Memory: 2, VCPUs: 1, DiskPath: "/x.qcow2", UUID: "11111111-2222-4333-8444-555555555555", Firmware: FirmwareBIOS}
	x := domainXML(cfg)
	for _, want := range []string{
		"<uuid>11111111-2222-4333-8444-555555555555</uuid>",
		"<sysinfo type='smbios'><system><entry name='serial'>ds=nocloud;h=web;i=11111111-2222-4333-8444-555555555555</entry></system></sysinfo>",
		"<smbios mode='sysinfo'/>",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("XML missing %q:\n%s", want, x)
		}
	}
	// <os> order: type, smbios, boot.
	ti, si, bi := strings.Index(x, "<type "), strings.Index(x, "<smbios"), strings.Index(x, "<boot ")
	if !(ti < si && si < bi) {
		t.Errorf("os order wrong: type=%d smbios=%d boot=%d", ti, si, bi)
	}
	if strings.Count(x, "<os>") != 1 {
		t.Errorf("want exactly one <os>")
	}
}

func TestDomainXMLNoUUIDOmitsIdentity(t *testing.T) {
	x := domainXML(VMConfig{Name: "web"})
	if strings.Contains(x, "<uuid>") || strings.Contains(x, "smbios") {
		t.Errorf("no UUID should emit no identity:\n%s", x)
	}
}

func TestHostnameFromName(t *testing.T) {
	if got := hostnameFromName("my_vm_1"); got != "my-vm-1" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("a", 100)
	if got := hostnameFromName(long); len(got) != 63 {
		t.Errorf("len %d, want 63", len(got))
	}
	x := domainXML(VMConfig{Name: "my_vm", UUID: "u"})
	if !strings.Contains(x, "h=my-vm;") {
		t.Errorf("underscore not mapped in serial:\n%s", x)
	}
	if !strings.Contains(x, "<name>my_vm</name>") {
		t.Errorf("domain name must keep underscore:\n%s", x)
	}
}

func TestDomainXMLEscapesName(t *testing.T) {
	x := domainXML(VMConfig{Name: `a<b>&"'c`, UUID: "u"})
	if strings.Contains(x, "a<b>") {
		t.Errorf("name not escaped:\n%s", x)
	}
	if !strings.Contains(x, "<name>a&lt;b&gt;&amp;") {
		t.Errorf("name element not escaped:\n%s", x)
	}
	// The serial entry is text content; the name must be escaped there too.
	if !strings.Contains(x, "h=a&lt;b&gt;&amp;") {
		t.Errorf("serial not escaped:\n%s", x)
	}
}

func TestPrepareDefineTwoUUIDs(t *testing.T) {
	cfg := VMConfig{Name: "web"}
	x1, c1 := prepareDefine(cfg)
	x2, c2 := prepareDefine(cfg)
	if !uuidV4.MatchString(c1.UUID) || !uuidV4.MatchString(c2.UUID) {
		t.Fatalf("not v4 uuids: %q %q", c1.UUID, c2.UUID)
	}
	if c1.UUID == c2.UUID {
		t.Errorf("two defines gave the same uuid %s", c1.UUID)
	}
	if !strings.Contains(x1, c1.UUID) || !strings.Contains(x2, c2.UUID) {
		t.Errorf("uuid not in xml")
	}
	if cfg.UUID != "" {
		t.Errorf("input config mutated")
	}
}

func TestPrepareDefineKeepsGivenUUID(t *testing.T) {
	u := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	x, c := prepareDefine(VMConfig{Name: "web", UUID: u})
	if c.UUID != u || !strings.Contains(x, "<uuid>"+u+"</uuid>") {
		t.Errorf("given uuid not kept:\n%s", x)
	}
}
