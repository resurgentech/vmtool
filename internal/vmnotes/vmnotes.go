// Package vmnotes holds the rules of a VM's notes: what vmtool records about
// a VM besides what its host reports, its origin (what it was made from,
// recorded once at create) and its comment. The notes are kept in the store
// only, never in a VM's libvirt definition (locked decision "VM inventory"
// 4). The package imports neither libvirt nor the store, so the store (built
// without cgo), the store client side in internal/app and every surface share
// one set of rules.
package vmnotes

import (
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxCommentBytes is the longest comment, in bytes of UTF-8.
const MaxCommentBytes = 512

// Clone kinds of an origin: how the VM's boot disk was made.
const (
	CloneKindThin  = "thin"  // a snapshot of a template in a thin pool
	CloneKindBlock = "block" // a copy to a logical volume (a thick LV)
	CloneKindFile  = "file"  // a copy to a qcow2 file
	CloneKindNone  = "none"  // booted in place (--noclone)
)

// ValidCloneKind reports whether k is one of the clone kinds.
func ValidCloneKind(k string) bool {
	switch k {
	case CloneKindThin, CloneKindBlock, CloneKindFile, CloneKindNone:
		return true
	}
	return false
}

// Origin is what a VM was made from and how. Created is the creation time
// on the workstation's clock.
type Origin struct {
	Image       string
	ImageSHA256 string
	Template    string
	CloneKind   string
	Pool        string
	Created     time.Time
}

// ValidateComment checks a comment: at most MaxCommentBytes bytes of valid
// UTF-8 on one line, with no control characters (U+2028 and U+2029 count as
// line breaks). Empty is valid (it clears). The message names the field,
// never the value.
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
