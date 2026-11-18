package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// pool list has a TYPE column; an unknown type (an older daemon) prints "-".
func TestWritePoolTableType(t *testing.T) {
	var b bytes.Buffer
	err := writePoolTable(&b, []vmtool.PoolInfo{
		{Name: "vmtool", Path: "/srv/vmtool", Active: true, Type: "dir"},
		{Name: "nvme", Path: "/dev/vg-test", Active: true, Type: "logical"},
		{Name: "old", Path: "/srv/old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("output:\n%s", b.String())
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "NAME TYPE PATH ACTIVE" {
		t.Errorf("header = %q", lines[0])
	}
	for i, want := range []string{"vmtool dir /srv/vmtool yes", "nvme logical /dev/vg-test yes", "old - /srv/old no"} {
		if got := strings.Join(strings.Fields(lines[i+1]), " "); got != want {
			t.Errorf("row %d = %q, want %q", i, got, want)
		}
	}
}
