package app

import (
	"strings"
	"testing"
)

func TestParseVMRef(t *testing.T) {
	ref, err := ParseVMRef("jetfire/web", "jetfire")
	if err != nil || ref != (VMRef{Host: "jetfire", Name: "web"}) || ref.String() != "jetfire/web" {
		t.Fatalf("ParseVMRef = %+v %q, %v", ref, ref.String(), err)
	}
	// Another host parses; the resolver decides whether it is served.
	if ref, err := ParseVMRef("veritech01/web", "jetfire"); err != nil || ref.Host != "veritech01" {
		t.Fatalf("other host: %+v, %v", ref, err)
	}

	_, err = ParseVMRef("web", "jetfire")
	if err == nil || err.Error() != `"web": VM names are host/vm; did you mean jetfire/web?` {
		t.Fatalf("bare name: %v", err)
	}
	if ae := asAppErr(t, err); ae.Kind != KindInvalid {
		t.Fatalf("bare name kind %v", ae.Kind)
	}
	for _, bad := range []string{"", "/", "jetfire/", "/web", "a/b/c", "../web", "jetfire/..", "jetfire/a b"} {
		if _, err := ParseVMRef(bad, "jetfire"); err == nil {
			t.Errorf("%q accepted", bad)
		} else if strings.Contains(err.Error(), "did you mean") && strings.Contains(bad, "/") {
			t.Errorf("%q: suggestion for a name that has a host: %v", bad, err)
		}
	}
}
