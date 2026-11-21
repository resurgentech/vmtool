package vmtool

import (
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Export volumes (tranche 13): the thick vmexp- LVs the helper's export makes.
// The helper is the stub sudo of thin_test.go; nothing real runs.

const expTags = "vmtool_export,vmtool_name=proj,vmtool_from=vmtpl-000000000001,vmtool_bytes=1000,vmtool_sha256=" + shaA

func TestThinParseReportExportVolumes(t *testing.T) {
	rep := reportJSON(
		lvJSON("thin-test", "vg-test", "twi-aotz--", sz60, "", "", "12.34", "1.00", "vmtool_thinpool"),
		lvJSON("vmtpl-000000000001", "vg-test", "Vwi-a-tz--", sz10, "thin-test", "", "50.00", "",
			"vmtool_template,vmtool_name=proj,vmtool_origin=saved"),
		lvJSON("vmexp-000000000001", "vg-test", "-wi-a-----", sz20, "", "", "", "", expTags),
		// not exports: no tag, a wrong name, a thin LV carrying the tag
		lvJSON("vmexp-000000000002", "vg-test", "-wi-a-----", sz20, "", "", "", "", "other"),
		lvJSON("notmine", "vg-test", "-wi-a-----", sz20, "", "", "", "", "vmtool_export"),
		lvJSON("vmexp-000000000003", "vg-test", "Vwi-a-tz--", sz10, "thin-test", "", "", "", "vmtool_export"),
	)
	pools, vols, err := ParseThinReport([]byte(rep))
	if err != nil {
		t.Fatal(err)
	}
	var got []ThinVolume
	for _, v := range vols {
		if v.Kind == ThinKindExport {
			got = append(got, v)
		}
	}
	if len(got) != 1 || got[0].Name != "vmexp-000000000001" || got[0].VG != "vg-test" || got[0].Pool != "" ||
		got[0].SizeBytes != 20*tGiB || got[0].Created.IsZero() {
		t.Fatalf("export volumes = %+v", got)
	}
	// a thick LV never counts toward what the pool has promised (the 20 GiB is the template and the thin LV)
	if pools[0].VirtualBytes != 20*tGiB {
		t.Errorf("VirtualBytes = %d", pools[0].VirtualBytes)
	}
	for _, tp := range ThinTemplates(vols) {
		if tp.LV != "vmtpl-000000000001" {
			t.Errorf("template list holds %q", tp.LV)
		}
	}
	e := ExportFromVolume(got[0])
	want := ThinExport{Name: "vmexp-000000000001", VG: "vg-test", TemplateName: "proj", From: "vmtpl-000000000001",
		Bytes: 1000, SHA256: shaA, SizeBytes: 20 * tGiB, Created: got[0].Created}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("ExportFromVolume = %+v\nwant %+v", e, want)
	}
	if !e.Complete() {
		t.Error("an export with vmtool_bytes is complete")
	}
	if (ThinExport{Name: "vmexp-000000000001"}).Complete() {
		t.Error("an export without vmtool_bytes is not complete")
	}
}

func TestThinExportName(t *testing.T) {
	for _, ok := range []string{"vmexp-000000000001", "vmexp-abcdef012345"} {
		if !ValidExportLVName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "vmexp-", "vmexp-00000000000", "vmexp-0000000000011", "vmexp-00000000000G", "VMEXP-000000000001", "vmtpl-000000000001"} {
		if ValidExportLVName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	n, err := NewExportLVName(strings.NewReader("\x00\x00\x00\x00\x00\x01"))
	if err != nil || n != "vmexp-000000000001" {
		t.Errorf("NewExportLVName = %q, %v", n, err)
	}
	if _, err := NewExportLVName(strings.NewReader("ab")); err == nil {
		t.Error("a short random source was accepted")
	}
}

// The listing rule: vmtool's libvirt volume listings leave vmexp- LVs out.
func TestIsExportVolumeName(t *testing.T) {
	for name, want := range map[string]bool{
		"vmexp-000000000001": true, "vmexp-whatever": true, "vmexp-": true,
		"img1.qcow2": false, "vm1": false, "vmtpl-000000000001": false, "myvmexp-1": false,
	} {
		if got := IsExportVolumeName(name); got != want {
			t.Errorf("IsExportVolumeName(%q) = %v", name, got)
		}
	}
	got := FilterImageNames([]string{"a.qcow2", "vmexp-000000000001", "vmexp-x.qcow2", "b.qcow2", "c.raw"})
	if !reflect.DeepEqual(got, []string{"a.qcow2", "b.qcow2"}) {
		t.Errorf("FilterImageNames = %q", got)
	}
}

func TestParseExportProgress(t *testing.T) {
	for _, c := range []struct {
		in   string
		pct  float64
		ok   bool
		name string
	}{
		{"    (12.34/100%)", 12.34, true, "qemu-img line"},
		{"    (100.00/100%)\r", 100, true, "with CR"},
		{"(0.00/100%)", 0, true, "zero"},
		{"    (5.00/100%)    (6.00/100%)", 6, true, "last wins"},
		{"", 0, false, "empty"},
		{"warning: x", 0, false, "text"},
		{"(abc/100%)", 0, false, "not a number"},
		{"(101.00/100%)", 0, false, "over 100"},
		{"(-1.00/100%)", 0, false, "negative"},
	} {
		pct, ok := ParseExportProgress(c.in)
		if ok != c.ok || pct != c.pct {
			t.Errorf("%s: ParseExportProgress(%q) = %v, %v", c.name, c.in, pct, ok)
		}
	}
}

func TestExportRoomError(t *testing.T) {
	e := &ExportRoomError{Template: "t1-tpl", VG: "vg-test", Need: 21_500_000_000, Free: 39_800_000_000, Reserve: 30_000_000_000}
	if !errors.Is(e, ErrThinNoRoom) {
		t.Error("an ExportRoomError must match ErrThinNoRoom")
	}
	want := "export of t1-tpl needs 21.5 GB in volume group vg-test; 9.8 GB is free outside the thin pool after keeping 30.0 GB for it to grow; nothing was written"
	if e.Error() != want {
		t.Errorf("message = %q\nwant      %q", e.Error(), want)
	}
	// free less than the reserve reads as none free
	e = &ExportRoomError{Template: "t1-tpl", VG: "vg-test", Need: 1_000_000_000, Free: 1, Reserve: 30_000_000_000}
	if !strings.Contains(e.Error(), "0.0 GB is free") {
		t.Errorf("message = %q", e.Error())
	}
}

func TestThinClientExportThickArgvAndProgress(t *testing.T) {
	log := thinStub(t)
	t.Setenv("STUB_OUT", "    (1.50/100%)\r    (50.00/100%)\r    (100.00/100%)\r\n")
	var got []float64
	err := (&Thin{}).ExportThick("vg-test", "vmtpl-000000000001", "vmexp-000000000001",
		[]string{"vmtool_export", "vmtool_name=proj", "vmtool_from=vmtpl-000000000001"},
		func(p float64) { got = append(got, p) })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-n", "/usr/local/libexec/vmtool-thin", "export", "vg-test", "vmtpl-000000000001", "vmexp-000000000001",
		"vmtool_export", "vmtool_name=proj", "vmtool_from=vmtpl-000000000001"}
	if a := readCall(t, log); !reflect.DeepEqual(a, want) {
		t.Fatalf("argv = %q\nwant   %q", a, want)
	}
	if !reflect.DeepEqual(got, []float64{1.5, 50, 100}) {
		t.Errorf("progress = %v", got)
	}
	// a nil callback is fine
	if err := (&Thin{}).ExportThick("vg-test", "vmtpl-000000000001", "vmexp-000000000001", []string{"vmtool_export"}, nil); err != nil {
		t.Errorf("nil callback: %v", err)
	}
}

func TestThinClientExportThickRejectsBeforeRunning(t *testing.T) {
	log := thinStub(t)
	th := &Thin{}
	for name, f := range map[string]func() error{
		"bad vg": func() error {
			return th.ExportThick("a b", "vmtpl-000000000001", "vmexp-000000000001", []string{"vmtool_export"}, nil)
		},
		"bad template": func() error {
			return th.ExportThick("vg-test", "../x", "vmexp-000000000001", []string{"vmtool_export"}, nil)
		},
		"bad name": func() error {
			return th.ExportThick("vg-test", "vmtpl-000000000001", "vm1", []string{"vmtool_export"}, nil)
		},
		"no export tag": func() error {
			return th.ExportThick("vg-test", "vmtpl-000000000001", "vmexp-000000000001", []string{"vmtool_name=proj"}, nil)
		},
		"bad tag": func() error {
			return th.ExportThick("vg-test", "vmtpl-000000000001", "vmexp-000000000001", []string{"vmtool_export", "x y"}, nil)
		},
	} {
		if err := f(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if b, _ := readFileOrEmpty(log); b != "" {
		t.Errorf("the helper ran: %q", b)
	}
}

func TestThinClientExportThickNoRoom(t *testing.T) {
	thinStub(t)
	t.Setenv("STUB_RC", "1")
	t.Setenv("STUB_ERR", "vmtool-thin: no room: need=21474836480 free=40000000000 reserve=30000000000")
	err := (&Thin{}).ExportThick("vg-test", "vmtpl-000000000001", "vmexp-000000000001", []string{"vmtool_export"}, nil)
	if !errors.Is(err, ErrThinNoRoom) {
		t.Fatalf("err = %v, want ErrThinNoRoom", err)
	}
	var re *ExportRoomError
	if !errors.As(err, &re) || re.Need != 21474836480 || re.Free != 40000000000 || re.Reserve != 30000000000 || re.VG != "vg-test" {
		t.Fatalf("room error = %+v", re)
	}
}

func TestThinClientExportThickOtherFailure(t *testing.T) {
	thinStub(t)
	t.Setenv("STUB_RC", "1")
	t.Setenv("STUB_ERR", "vmtool-thin: convert failed")
	err := (&Thin{}).ExportThick("vg-test", "vmtpl-000000000001", "vmexp-000000000001", []string{"vmtool_export"}, nil)
	if err == nil || errors.Is(err, ErrThinNoRoom) || !strings.Contains(err.Error(), "convert failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestThinClientSend(t *testing.T) {
	log := thinStub(t)
	t.Setenv("STUB_OUT", "0123456789")
	r, err := (&Thin{}).Send("vg-test", "vmexp-000000000001", 10)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "0123456789" {
		t.Fatalf("read %q, %v", b, err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	want := []string{"-n", "/usr/local/libexec/vmtool-thin", "send", "vg-test", "vmexp-000000000001"}
	if a := readCall(t, log); !reflect.DeepEqual(a, want) {
		t.Errorf("argv = %q\nwant   %q", a, want)
	}
}

// A stream shorter than the tag says is an error, never a quiet EOF.
func TestThinClientSendShortReadIsAnError(t *testing.T) {
	thinStub(t)
	t.Setenv("STUB_OUT", "01234")
	r, err := (&Thin{}).Send("vg-test", "vmexp-000000000001", 10)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err == nil || err == io.EOF || string(b) != "01234" || !strings.Contains(err.Error(), "5 of 10") {
		t.Fatalf("read %q, %v", b, err)
	}
	r.Close()
}

func TestThinClientSendHelperFailure(t *testing.T) {
	thinStub(t)
	t.Setenv("STUB_OUT", "0123456789")
	t.Setenv("STUB_RC", "1")
	t.Setenv("STUB_ERR", "vmtool-thin: dd failed")
	r, err := (&Thin{}).Send("vg-test", "vmexp-000000000001", 10)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r)
	if err == nil || !strings.Contains(err.Error(), "dd failed") {
		t.Fatalf("a failing helper after all bytes = %v", err)
	}
	r.Close()
}

func TestThinClientSendEarlyClose(t *testing.T) {
	thinStub(t)
	t.Setenv("STUB_OUT", "0123456789")
	r, err := (&Thin{}).Send("vg-test", "vmexp-000000000001", 10)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	r.Close() // must return, and not hang
	r.Close() // twice is fine
}

func TestThinClientSendRejects(t *testing.T) {
	log := thinStub(t)
	th := &Thin{}
	if _, err := th.Send("vg-test", "vm1", 10); err == nil {
		t.Error("a non-vmexp name was accepted")
	}
	if _, err := th.Send("vg-test", "vmexp-000000000001", 0); err == nil {
		t.Error("a zero length was accepted")
	}
	if b, _ := readFileOrEmpty(log); b != "" {
		t.Errorf("the helper ran: %q", b)
	}
}

func readFileOrEmpty(p string) (string, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}
