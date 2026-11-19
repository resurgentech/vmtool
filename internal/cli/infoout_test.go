package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// vmtool info prints the UUID, and "unknown" when the daemon did not send one.
func TestWriteVMInfoUUID(t *testing.T) {
	var b bytes.Buffer
	writeVMInfo(&b, &vmtool.VMInfo{Name: "a", UUID: "00000000-0000-4000-8000-000000000001"})
	if !strings.Contains(b.String(), "UUID:      00000000-0000-4000-8000-000000000001\n") {
		t.Errorf("output:\n%s", b.String())
	}
	b.Reset()
	writeVMInfo(&b, &vmtool.VMInfo{Name: "a"})
	if !strings.Contains(b.String(), "UUID:      unknown\n") {
		t.Errorf("output:\n%s", b.String())
	}
}
