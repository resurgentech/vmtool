package vmtool

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var testOrigin = &VMOrigin{
	Image:       "img1",
	ImageSHA256: strings.Repeat("a", 64),
	Template:    "img1",
	TemplateLV:  "vmtpl-aaaaaaaaaaaa",
	CloneKind:   CloneKindThin,
	Pool:        "nvme-thin",
	Created:     time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC),
}

// The element is vmtool's own namespace, one element holding origin and
// comment; builder and parser round-trip, with XML-special characters.
func TestMetadataRoundTrip(t *testing.T) {
	for _, c := range []struct {
		name    string
		origin  *VMOrigin
		comment string
	}{
		{"both", testOrigin, "placeholder comment"},
		{"origin only", testOrigin, ""},
		{"comment only", nil, "placeholder comment"},
		{"special characters", testOrigin, `a < b && c > d "q" 'r' </comment><x/> ]]> ünï ✓`},
		{"noclone origin", &VMOrigin{Image: "img1", CloneKind: CloneKindNone, Created: testOrigin.Created}, "c"},
	} {
		t.Run(c.name, func(t *testing.T) {
			el := MetadataElement(c.origin, c.comment)
			if !strings.Contains(el, MetadataNS) {
				t.Fatalf("element lacks vmtool's namespace:\n%s", el)
			}
			o, comment := ParseMetadata(el)
			if !reflect.DeepEqual(o, c.origin) || comment != c.comment {
				t.Errorf("parsed (%+v, %q), want (%+v, %q)\n%s", o, comment, c.origin, c.comment, el)
			}
			// inside a whole domain, as Info reads it
			dom := "<domain type='kvm'>\n<name>vm1</name>\n<metadata>\n" + el + "\n</metadata>\n</domain>"
			o, comment = ParseMetadata(dom)
			if !reflect.DeepEqual(o, c.origin) || comment != c.comment {
				t.Errorf("parsed from domain (%+v, %q), want (%+v, %q)", o, comment, c.origin, c.comment)
			}
		})
	}
}

func TestMetadataElementEmpty(t *testing.T) {
	if el := MetadataElement(nil, ""); el != "" {
		t.Errorf("no origin and no comment wrote %q", el)
	}
}

// A VM with no element (made before this, or outside vmtool) reports nothing;
// so does a domain with other metadata, and garbage.
func TestParseMetadataAbsent(t *testing.T) {
	for _, x := range []string{
		"",
		"<domain type='kvm'><name>vm1</name></domain>",
		"<domain><metadata><other xmlns='urn:other'><origin image='x'/></other></metadata></domain>",
		"<domain><metadata><vm xmlns='urn:other'><comment>no</comment></vm></metadata></domain>",
		"<not xml",
	} {
		if o, c := ParseMetadata(x); o != nil || c != "" {
			t.Errorf("ParseMetadata(%q) = (%+v, %q), want nothing", x, o, c)
		}
	}
}

// libvirt may write the element back with its own prefix and quoting.
func TestParseMetadataLibvirtSpelling(t *testing.T) {
	x := `<domain type='kvm'><metadata><v:vm xmlns:v='` + MetadataNS + `'><origin image="img1" clone_kind="file" created="2026-11-21T04:05:06Z"/><comment>placeholder comment</comment></v:vm></metadata></domain>`
	o, c := ParseMetadata(x)
	want := &VMOrigin{Image: "img1", CloneKind: CloneKindFile, Created: testOrigin.Created}
	if !reflect.DeepEqual(o, want) || c != "placeholder comment" {
		t.Errorf("got (%+v, %q)", o, c)
	}
}

// domainXML writes the element when there is an origin or a comment, and none
// otherwise; the result is still the domain the rest of the tests know.
func TestDomainXMLMetadata(t *testing.T) {
	cfg := DefaultConfig("vm1", "/var/lib/libvirt/images/vm1.qcow2")
	cfg.UUID = "00000000-0000-4000-8000-000000000001"
	if x := domainXML(cfg); strings.Contains(x, "<metadata>") {
		t.Errorf("a VM with neither wrote metadata:\n%s", x)
	}
	cfg.Origin = testOrigin
	cfg.Comment = "a & b"
	x := domainXML(cfg)
	if strings.Count(x, "<metadata>") != 1 {
		t.Fatalf("want one <metadata>:\n%s", x)
	}
	o, c := ParseMetadata(x)
	if !reflect.DeepEqual(o, testOrigin) || c != "a & b" {
		t.Errorf("domainXML metadata reads back (%+v, %q)", o, c)
	}
}

// The redefine in MigrateDisk, and the attach-device and resize paths, edit
// or read the domain without dropping the element: redefineDiskPath is a
// plain path swap over GetXMLDesc, so the element survives it.
func TestRedefineDiskPathKeepsMetadata(t *testing.T) {
	cfg := DefaultConfig("vm1", "/pool/a/vm1.qcow2")
	cfg.UUID = "00000000-0000-4000-8000-000000000001"
	cfg.Origin = testOrigin
	cfg.Comment = "placeholder comment"
	before := domainXML(cfg)
	after := redefineDiskPath(before, "/pool/a/vm1.qcow2", "/pool/b/vm1.qcow2")
	if !strings.Contains(after, "/pool/b/vm1.qcow2") {
		t.Fatalf("path not swapped:\n%s", after)
	}
	o, c := ParseMetadata(after)
	if !reflect.DeepEqual(o, testOrigin) || c != "placeholder comment" {
		t.Errorf("after redefine: (%+v, %q)", o, c)
	}
}

// Setting a comment rewrites the element with the origin kept; an empty
// comment on a VM with no origin leaves no element.
func TestReplaceComment(t *testing.T) {
	el := MetadataElement(testOrigin, "old")
	got := ReplaceComment(el, "new < comment")
	o, c := ParseMetadata(got)
	if !reflect.DeepEqual(o, testOrigin) || c != "new < comment" {
		t.Errorf("ReplaceComment = (%+v, %q)", o, c)
	}
	o, c = ParseMetadata(ReplaceComment(el, ""))
	if !reflect.DeepEqual(o, testOrigin) || c != "" {
		t.Errorf("cleared = (%+v, %q), want the origin kept", o, c)
	}
	if got := ReplaceComment("", "x"); !strings.Contains(got, "<comment>x</comment>") {
		t.Errorf("no element before: %q", got)
	}
	if got := ReplaceComment(MetadataElement(nil, "old"), ""); got != "" {
		t.Errorf("clearing the only content left %q, want no element", got)
	}
	if got := ReplaceComment("", ""); got != "" {
		t.Errorf("empty on nothing = %q", got)
	}
}

func TestValidateComment(t *testing.T) {
	ok := []string{"", "placeholder comment", "ünï ✓", strings.Repeat("a", 512), strings.Repeat("é", 256)}
	for _, s := range ok {
		if err := ValidateComment(s); err != nil {
			t.Errorf("ValidateComment(%.20q) = %v", s, err)
		}
	}
	bad := map[string]string{
		"too long":      strings.Repeat("a", 513),
		"too long utf8": strings.Repeat("é", 257),
		"newline":       "a\nb",
		"carriage":      "a\rb",
		"tab":           "a\tb",
		"nul":           "a\x00b",
		"escape":        "a\x1bb",
		"del":           "a\x7fb",
		"c1":            "a\u0085b",
		"line sep":      "a b",
		"bad utf8":      "a\xffb",
	}
	for name, s := range bad {
		err := ValidateComment(s)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "comment") {
			t.Errorf("%s: error %q does not name the field", name, err)
		}
	}
}

func TestValidateOrigin(t *testing.T) {
	if err := ValidateOrigin(nil); err != nil {
		t.Errorf("nil: %v", err)
	}
	if err := ValidateOrigin(testOrigin); err != nil {
		t.Errorf("good origin: %v", err)
	}
	for name, o := range map[string]VMOrigin{
		"clone_kind":   {CloneKind: "copy"},
		"image_sha256": {CloneKind: CloneKindFile, ImageSHA256: "zz"},
		"image":        {CloneKind: CloneKindFile, Image: "a\nb"},
		"pool":         {CloneKind: CloneKindFile, Pool: strings.Repeat("p", 300)},
	} {
		err := ValidateOrigin(&o)
		if err == nil || !strings.Contains(err.Error(), "origin."+name) {
			t.Errorf("%s: error %v does not name origin.%s", name, err, name)
		}
	}
}
