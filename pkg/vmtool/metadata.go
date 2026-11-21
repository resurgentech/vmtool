package vmtool

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"libvirt.org/go/libvirt"
)

// Domain metadata: one element in vmtool's own XML namespace, kept inside the
// VM's own definition (libvirt's <metadata>). It holds what a VM was made
// from (the origin, known only when vmtool creates it) and the user's comment.
// It survives daemon restarts and host reboots and goes with the VM.
//
//	<vmtool:vm xmlns:vmtool="urn:vmtool:vm:1">
//	  <origin image="img1" image_sha256="…" template="…" template_lv="…"
//	          clone_kind="thin" pool="…" created="2026-11-21T04:05:06Z"/>
//	  <comment>text</comment>
//	</vmtool:vm>
const (
	// MetadataNS is the namespace of the element.
	MetadataNS = "urn:vmtool:vm:1"
	// MetadataPrefix is the prefix libvirt's metadata call is given.
	MetadataPrefix = "vmtool"
)

// Clone kinds of VMOrigin.CloneKind.
const (
	CloneKindThin  = "thin"  // a snapshot of a template in a thin pool
	CloneKindBlock = "block" // a copy to a logical volume (a thick LV)
	CloneKindFile  = "file"  // a copy to a qcow2 file
	CloneKindNone  = "none"  // booted in place (--noclone)
)

// MaxCommentBytes is the longest comment, in bytes of UTF-8.
const MaxCommentBytes = 512

// maxOriginField is the longest value of an origin text field.
const maxOriginField = 256

// VMOrigin is what a VM was made from and how.
type VMOrigin struct {
	Image       string    `json:"image,omitempty"`
	ImageSHA256 string    `json:"image_sha256,omitempty"`
	Template    string    `json:"template,omitempty"`
	TemplateLV  string    `json:"template_lv,omitempty"`
	CloneKind   string    `json:"clone_kind"`
	Pool        string    `json:"pool,omitempty"`
	Created     time.Time `json:"created,omitzero"`
}

// metaVM is the element as the parser reads it.
type metaVM struct {
	XMLName xml.Name `xml:"urn:vmtool:vm:1 vm"`
	Origin  *struct {
		Image       string `xml:"image,attr"`
		ImageSHA256 string `xml:"image_sha256,attr"`
		Template    string `xml:"template,attr"`
		TemplateLV  string `xml:"template_lv,attr"`
		CloneKind   string `xml:"clone_kind,attr"`
		Pool        string `xml:"pool,attr"`
		Created     string `xml:"created,attr"`
	} `xml:"origin"`
	Comment string `xml:"comment"`
}

// MetadataElement is vmtool's metadata element for an origin and a comment;
// "" when there is neither. A pure function: the one place the element is
// written.
func MetadataElement(o *VMOrigin, comment string) string {
	if o == nil && comment == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<%s:vm xmlns:%s='%s'>\n", MetadataPrefix, MetadataPrefix, MetadataNS)
	if o != nil {
		b.WriteString("      <origin")
		for _, a := range []struct{ k, v string }{
			{"image", o.Image}, {"image_sha256", o.ImageSHA256}, {"template", o.Template},
			{"template_lv", o.TemplateLV}, {"clone_kind", o.CloneKind}, {"pool", o.Pool},
		} {
			if a.v != "" {
				fmt.Fprintf(&b, " %s='%s'", a.k, xmlEscape(a.v))
			}
		}
		if !o.Created.IsZero() {
			fmt.Fprintf(&b, " created='%s'", o.Created.UTC().Format(time.RFC3339))
		}
		b.WriteString("/>\n")
	}
	if comment != "" {
		fmt.Fprintf(&b, "      <comment>%s</comment>\n", xmlEscape(comment))
	}
	fmt.Fprintf(&b, "    </%s:vm>", MetadataPrefix)
	return b.String()
}

// metadataBlock is the <metadata> block of a domain; "" without an element.
func metadataBlock(o *VMOrigin, comment string) string {
	el := MetadataElement(o, comment)
	if el == "" {
		return ""
	}
	return "  <metadata>\n    " + el + "\n  </metadata>\n"
}

// ParseMetadata reads vmtool's element out of a domain's XML, or out of the
// bare element. A domain without one, other metadata, or text that is not XML
// gives no origin and no comment. A pure function.
func ParseMetadata(x string) (*VMOrigin, string) {
	d := xml.NewDecoder(strings.NewReader(x))
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, ""
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Space != MetadataNS || se.Name.Local != "vm" {
			continue
		}
		var v metaVM
		if err := d.DecodeElement(&v, &se); err != nil {
			return nil, ""
		}
		var o *VMOrigin
		if v.Origin != nil {
			o = &VMOrigin{
				Image: v.Origin.Image, ImageSHA256: v.Origin.ImageSHA256, Template: v.Origin.Template,
				TemplateLV: v.Origin.TemplateLV, CloneKind: v.Origin.CloneKind, Pool: v.Origin.Pool,
			}
			if t, err := time.Parse(time.RFC3339, v.Origin.Created); err == nil {
				o.Created = t.UTC()
			}
		}
		return o, v.Comment
	}
}

// ReplaceComment is the element el with its comment replaced (origin kept);
// "" when nothing is left. el may be "" (a VM with no element yet).
func ReplaceComment(el, comment string) string {
	o, _ := ParseMetadata(el)
	return MetadataElement(o, comment)
}

// ValidateComment checks a comment: at most MaxCommentBytes bytes of valid
// UTF-8 on one line, with no control characters. Empty is valid (it clears).
// The message names the field.
func ValidateComment(s string) error {
	if len(s) > MaxCommentBytes {
		return fmt.Errorf("comment is %d bytes; at most %d", len(s), MaxCommentBytes)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("comment is not valid UTF-8")
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return fmt.Errorf("comment must be one line with no control characters (found U+%04X)", r)
		}
	}
	return nil
}

// ValidateOrigin checks an origin that came over the wire: a known clone kind,
// a digest that is 64 lowercase hex or empty, and text fields that are short
// and carry no control characters. The message names origin.<field>.
func ValidateOrigin(o *VMOrigin) error {
	if o == nil {
		return nil
	}
	switch o.CloneKind {
	case CloneKindThin, CloneKindBlock, CloneKindFile, CloneKindNone:
	default:
		return fmt.Errorf("origin.clone_kind %q must be thin, block, file or none", o.CloneKind)
	}
	if o.ImageSHA256 != "" {
		ok := len(o.ImageSHA256) == 64
		for _, c := range o.ImageSHA256 {
			if !strings.ContainsRune("0123456789abcdef", c) {
				ok = false
			}
		}
		if !ok {
			return fmt.Errorf("origin.image_sha256 must be 64 lowercase hex characters")
		}
	}
	for _, f := range []struct{ name, v string }{
		{"image", o.Image}, {"template", o.Template}, {"template_lv", o.TemplateLV}, {"pool", o.Pool},
	} {
		if len(f.v) > maxOriginField {
			return fmt.Errorf("origin.%s is %d bytes; at most %d", f.name, len(f.v), maxOriginField)
		}
		if !utf8.ValidString(f.v) {
			return fmt.Errorf("origin.%s is not valid UTF-8", f.name)
		}
		for _, r := range f.v {
			if unicode.IsControl(r) {
				return fmt.Errorf("origin.%s must not contain control characters", f.name)
			}
		}
	}
	return nil
}

// SetComment sets or clears a VM's comment through libvirt's metadata call: on
// the persistent definition, and on the running domain when it runs, without
// redefining the VM. The origin in the same element is kept.
func (m *Manager) SetComment(name, comment string) error {
	if err := ValidateComment(comment); err != nil {
		return err
	}
	dom, err := m.conn.LookupDomainByName(name)
	if err != nil {
		return fmt.Errorf("looking up domain %q: %w", name, err)
	}
	defer dom.Free()
	active, err := dom.IsActive()
	if err != nil {
		return fmt.Errorf("getting state: %w", err)
	}
	flags := libvirt.DOMAIN_AFFECT_CONFIG
	if active {
		flags |= libvirt.DOMAIN_AFFECT_LIVE
	}
	// A domain with no element answers an error here; that is "no element".
	cur, _ := dom.GetMetadata(libvirt.DOMAIN_METADATA_ELEMENT, MetadataNS, libvirt.DOMAIN_AFFECT_CONFIG)
	next := ReplaceComment(cur, comment)
	if next == "" && cur == "" {
		return nil
	}
	if err := dom.SetMetadata(libvirt.DOMAIN_METADATA_ELEMENT, next, MetadataPrefix, MetadataNS, flags); err != nil {
		return fmt.Errorf("setting comment: %w", err)
	}
	return nil
}
