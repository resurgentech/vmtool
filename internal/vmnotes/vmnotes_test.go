package vmnotes

import (
	"strings"
	"testing"
)

func TestValidateComment(t *testing.T) {
	ok := []string{"", "placeholder comment", "ünï ✓", strings.Repeat("a", 512), strings.Repeat("é", 256)}
	for _, s := range ok {
		if err := ValidateComment(s); err != nil {
			t.Errorf("ValidateComment(%.20q) = %v", s, err)
		}
	}
	bad := map[string]string{
		"too long":      strings.Repeat("a", MaxCommentBytes+1),
		"too long utf8": strings.Repeat("é", 257),
		"newline":       "a\nb",
		"carriage":      "a\rb",
		"tab":           "a\tb",
		"nul":           "a\x00b",
		"escape":        "a\x1bb",
		"del":           "a\x7fb",
		"c1":            "a\u0085b",
		"line sep":      "a b",
		"para sep":      "a b",
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

func TestValidCloneKind(t *testing.T) {
	for _, k := range []string{CloneKindThin, CloneKindBlock, CloneKindFile, CloneKindNone} {
		if !ValidCloneKind(k) {
			t.Errorf("%q refused", k)
		}
	}
	for _, k := range []string{"", "copy", "THIN"} {
		if ValidCloneKind(k) {
			t.Errorf("%q accepted", k)
		}
	}
}
