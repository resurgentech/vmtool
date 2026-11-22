package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

func i64(n int64) *int64     { return &n }
func f64(f float64) *float64 { return &f }

// tableRows renders the pool table and returns its lines (header first).
func tableRows(t *testing.T, pools []vmtool.PoolInfo) []string {
	t.Helper()
	var b bytes.Buffer
	if err := writePoolTable(&b, pools); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
}

// col returns the cell of row under the named header column.
func col(t *testing.T, header, row, name string) string {
	t.Helper()
	i := strings.Index(header, name)
	if i < 0 {
		t.Fatalf("no column %s in %q", name, header)
	}
	rest := header[i+len(name):]
	end := len(row)
	if j := strings.IndexFunc(rest, func(r rune) bool { return r != ' ' }); j >= 0 {
		end = i + len(name) + j
	}
	if i >= len(row) {
		return ""
	}
	if end > len(row) {
		end = len(row)
	}
	return strings.TrimSpace(row[i:end])
}

// pool list has TYPE and usage columns; unknown usage prints blank, an unknown
// type prints "-".
func TestWritePoolTableColumns(t *testing.T) {
	lines := tableRows(t, []vmtool.PoolInfo{
		{Name: "vmtool", Path: "/srv/vmtool", Active: true, Type: "dir", SizeBytes: i64(100 << 30), UsedBytes: i64(25 << 30)},
		{Name: "nvme-thin", Path: "/dev/vg-test", Active: true, Type: "thin", SizeBytes: i64(60 << 30), UsedBytes: i64(15 << 30),
			VirtualBytes: i64(30 << 30), DataPercent: f64(25), MetaPercent: f64(1.5)},
		{Name: "old", Path: "/srv/old"},
	})
	if len(lines) != 4 {
		t.Fatalf("lines = %q", lines)
	}
	h := lines[0]
	if got := strings.Join(strings.Fields(h), " "); got != "NAME TYPE SIZE USED VIRTUAL DATA% META% ACTIVE PATH" {
		t.Fatalf("header = %q", h)
	}
	want := []map[string]string{
		{"NAME": "vmtool", "TYPE": "dir", "SIZE": "100G", "USED": "25G", "VIRTUAL": "", "DATA%": "", "META%": "", "ACTIVE": "yes", "PATH": "/srv/vmtool"},
		{"NAME": "nvme-thin", "TYPE": "thin", "SIZE": "60G", "USED": "15G", "VIRTUAL": "30G", "DATA%": "25.0", "META%": "1.5", "ACTIVE": "yes", "PATH": "/dev/vg-test"},
		{"NAME": "old", "TYPE": "-", "SIZE": "", "USED": "", "VIRTUAL": "", "DATA%": "", "META%": "", "ACTIVE": "no", "PATH": "/srv/old"},
	}
	for i, w := range want {
		for c, v := range w {
			if got := col(t, h, lines[i+1], c); got != v {
				t.Errorf("row %d %s = %q, want %q (line %q)", i, c, got, v, lines[i+1])
			}
		}
	}
}

// An old daemon's pools carry no usage: every usage cell is blank.
func TestWritePoolTableOldDaemonBlanks(t *testing.T) {
	lines := tableRows(t, []vmtool.PoolInfo{{Name: "vmtool", Path: "/srv/vmtool", Active: true}})
	for _, c := range []string{"SIZE", "USED", "VIRTUAL", "DATA%", "META%"} {
		if got := col(t, lines[0], lines[1], c); got != "" {
			t.Errorf("%s = %q, want blank", c, got)
		}
	}
}

// A thin pool at or above 80 % data or metadata gets a "!" after its name; a
// libvirt pool never does, and 79.9 does not.
func TestWritePoolTableThinWarning(t *testing.T) {
	thin := func(name string, data, meta float64) vmtool.PoolInfo {
		return vmtool.PoolInfo{Name: name, Path: "/dev/vg-test", Active: true, Type: "thin", DataPercent: f64(data), MetaPercent: f64(meta)}
	}
	lines := tableRows(t, []vmtool.PoolInfo{
		thin("a", 80, 1), thin("b", 1, 80), thin("c", 79.9, 79.9), thin("d", 95, 95),
		{Name: "e", Type: "dir", DataPercent: f64(99)},
		{Name: "f", Type: "thin"}, // numbers unknown: no mark
	})
	wantMark := []bool{true, true, false, true, false, false}
	for i, m := range wantMark {
		name := col(t, lines[0], lines[i+1], "NAME")
		if got := strings.HasSuffix(name, "!"); got != m {
			t.Errorf("row %d name %q: marked = %v, want %v", i, name, got, m)
		}
	}
}

func TestFormatPoolBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0", 512: "512B", 1 << 10: "1K", 1536 << 10: "1.5M", 64424509440: "60G", 3 << 39: "1.5T",
	} {
		if got := formatPoolBytes(n); got != want {
			t.Errorf("formatPoolBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
