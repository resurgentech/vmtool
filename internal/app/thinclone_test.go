package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

var thinTestPools = []vmtool.PoolInfo{
	{Name: "vmtool", Path: "/pool", Type: "dir"},
	{Name: "nvme", Path: "/dev/vg-test", Type: "logical"},
	{Name: "thin-test", Path: "/dev/vg-thin", Type: "thin"},
}

func TestCloneCacheDefaultThin(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"thin", "/dev/vg-thin/vm1", "none"},
		{"thin, path not clean", "/dev/vg-thin//vm1", "none"},
		{"logical still none", "/dev/vg-test/vm1", "none"},
		{"dir still unsafe", "/pool/vm1.qcow2", "unsafe"},
		{"nested under a thin pool path", "/dev/vg-thin/a/vm1", "unsafe"},
	} {
		if got := cloneCacheDefault(thinTestPools, tc.path); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	// a thin pool whose path equals a logical pool's: both are none
	same := []vmtool.PoolInfo{{Name: "a", Path: "/dev/vg-x", Type: "logical"}, {Name: "b", Path: "/dev/vg-x", Type: "thin"}}
	if got := cloneCacheDefault(same, "/dev/vg-x/vm1"); got != "none" {
		t.Errorf("shared path: %q", got)
	}
}

// templateVMs is createVMs that also takes the image name with the clone.
type templateVMs struct {
	uefiVMs
	fromImage string
	calls     []string
	cloneErr  error
	extra     string
}

func (f *templateVMs) CloneImageFrom(image, base, name, pool string, size uint64) (string, error) {
	f.fromImage = image
	f.calls = append(f.calls, fmt.Sprintf("CloneImageFrom %s %s %s %s", image, base, name, pool))
	if f.cloneErr != nil {
		return "", f.cloneErr
	}
	return f.createVMs.CloneImage(base, name, pool, size)
}

func (f *templateVMs) CloneImage(base, name, pool string, size uint64) (string, error) {
	f.calls = append(f.calls, "CloneImage")
	return f.createVMs.CloneImage(base, name, pool, size)
}

func (f *templateVMs) CreateVolume(pool, name string, gb uint) (string, error) {
	f.calls = append(f.calls, "CreateVolume "+pool)
	return "/pool/" + name, nil
}

func createThin(t *testing.T, m Manager, in CreateVMInput) error {
	t.Helper()
	t.Chdir(t.TempDir())
	if in.Host == "" {
		in.Host = "hv1"
	}
	if in.Name == "" {
		in.Name = "vm1"
	}
	in.Image = "img1"
	in.NoWait = true
	_, err := (&Service{}).CreateVM(context.Background(), m, in)
	return err
}

func TestCreateUsesTemplateCloner(t *testing.T) {
	m := &templateVMs{uefiVMs: uefiVMs{createVMs: createVMs{pools: thinTestPools, clonePath: "/dev/vg-thin/vm1"}}}
	if err := createThin(t, m, CreateVMInput{Pool: "thin-test", DiskSizeGB: 20}); err != nil {
		t.Fatal(err)
	}
	if m.fromImage != "img1" || len(m.calls) != 1 || m.calls[0] != "CloneImageFrom img1 /pool/img1 vm1 thin-test" {
		t.Errorf("calls %q", m.calls)
	}
	if m.defined.DiskCache != "none" {
		t.Errorf("Define cache = %q, want none for a thin clone", m.defined.DiskCache)
	}
	if m.cloneSize != 20<<30 {
		t.Errorf("clone size = %d", m.cloneSize)
	}
	// a Manager without it is the plain CloneImage, as before
	plain := &uefiVMs{createVMs: createVMs{}}
	if err := createThin(t, plain, CreateVMInput{}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRefusesVmtplNameOnThinPool(t *testing.T) {
	m := &templateVMs{uefiVMs: uefiVMs{createVMs: createVMs{pools: thinTestPools}}}
	err := createThin(t, m, CreateVMInput{Pool: "thin-test", Name: "vmtpl-x"})
	if ae := asAppErr(t, err); ae.Kind != KindInvalid || !strings.Contains(err.Error(), "vmtpl-") {
		t.Fatalf("err = %v", err)
	}
	if len(m.calls) != 0 || m.defined.Name != "" {
		t.Errorf("something ran: %q %+v", m.calls, m.defined)
	}
	// a non-thin pool, or none named, is not the app's to refuse (the daemon
	// is the backstop for the host default)
	for _, pool := range []string{"vmtool", "nvme", ""} {
		m := &templateVMs{uefiVMs: uefiVMs{createVMs: createVMs{pools: thinTestPools}}}
		if err := createThin(t, m, CreateVMInput{Pool: pool, Name: "vmtpl-x"}); err != nil {
			t.Errorf("pool %q: %v", pool, err)
		}
	}
}

func TestCreateRefusesNocloneOnThinPool(t *testing.T) {
	m := &templateVMs{uefiVMs: uefiVMs{createVMs: createVMs{pools: thinTestPools}}}
	err := createThin(t, m, CreateVMInput{Pool: "thin-test", Noclone: true})
	if ae := asAppErr(t, err); ae.Kind != KindInvalid || !strings.Contains(err.Error(), "noclone") {
		t.Fatalf("err = %v", err)
	}
	if len(m.calls) != 0 || m.defined.Name != "" {
		t.Errorf("something ran: %q", m.calls)
	}
	// a non-thin pool named with --noclone goes on
	m = &templateVMs{uefiVMs: uefiVMs{createVMs: createVMs{pools: thinTestPools}}}
	if err := createThin(t, m, CreateVMInput{Pool: "vmtool", Noclone: true}); err != nil {
		t.Fatal(err)
	}
	// and with no pool named nothing is listed (as before)
	m = &templateVMs{uefiVMs: uefiVMs{createVMs: createVMs{poolsErr: fmt.Errorf("pools down")}}}
	if err := createThin(t, m, CreateVMInput{Noclone: true}); err != nil {
		t.Fatalf("noclone, no pool: %v", err)
	}
}

// An extra disk on a thin pool is refused before the clone, when the pools
// were listed anyway; nothing is left behind.
func TestCreateExtraDiskOnThinPoolRefusedBeforeClone(t *testing.T) {
	for name, in := range map[string]CreateVMInput{
		"extra pool": {Pool: "vmtool", ExtraDiskSizeGB: 5, ExtraDiskPool: "thin-test"},
		"main pool":  {Pool: "thin-test", ExtraDiskSizeGB: 5},
	} {
		m := &templateVMs{uefiVMs: uefiVMs{createVMs: createVMs{pools: thinTestPools}}}
		err := createThin(t, m, in)
		if ae := asAppErr(t, err); ae.Kind != KindInvalid || !strings.Contains(err.Error(), "thin") {
			t.Errorf("%s: err = %v", name, err)
		}
		if len(m.calls) != 0 {
			t.Errorf("%s: ran %q", name, m.calls)
		}
	}
	// a non-thin extra pool is fine
	m := &templateVMs{uefiVMs: uefiVMs{createVMs: createVMs{pools: thinTestPools}}}
	if err := createThin(t, m, CreateVMInput{Pool: "vmtool", ExtraDiskSizeGB: 5}); err != nil {
		t.Fatal(err)
	}
}

// A 409 from the clone (no room in the thin pool) reaches the caller as a
// conflict.
func TestCreateCloneConflictStaysConflict(t *testing.T) {
	m := &templateVMs{uefiVMs: uefiVMs{createVMs: createVMs{pools: thinTestPools}},
		cloneErr: &Error{Kind: KindConflict, Op: "clone", Err: fmt.Errorf("thin pool thin-test (vg-thin) has no room: data use is 85.00%%")}}
	err := createThin(t, m, CreateVMInput{Pool: "thin-test"})
	if ae := asAppErr(t, err); ae.Kind != KindConflict || !strings.Contains(err.Error(), "85.00%") {
		t.Fatalf("err = %v", err)
	}
	if m.defined.Name != "" {
		t.Error("Define ran after a failed clone")
	}
}

// ---- syncImage: the host has a template of that name ----

// tplVMs is a host that caches images but has none of img1 and answers
// TemplatePath(img1) with tplPath (a saved template) or not found; asked
// counts the TemplatePath calls.
type tplVMs struct {
	storeVMs
	tplPath string
	asked   int
}

func (f *tplVMs) TemplatePath(name string) (string, error) {
	f.asked++
	if f.tplPath == "" {
		return "", fmt.Errorf("image %q not found in any storage pool: %w", name, vmtool.ErrNotFound)
	}
	return f.tplPath, nil
}

func (f *tplVMs) CloneImageFrom(image, base, name, pool string, size uint64) (string, error) {
	return "/dev/vg-thin/" + name, nil
}

func TestSyncImageTemplateOnly(t *testing.T) {
	t.Chdir(t.TempDir())
	cache := filepath.Join(t.TempDir(), vmtool.ImageManifestFile)
	var lines []string
	svc := &Service{Images: LocalImages{Dir: t.TempDir(), Pools: notFound}, ImageCachePath: cache}
	m := &tplVMs{tplPath: "/dev/vg-thin/vmtpl-000000000001"}
	_, err := stubSvc(t, svc).CreateVM(context.Background(), m, CreateVMInput{
		Host: "hv1", Name: "web", Image: "img1",
		OnProgress: func(ev ProgressEvent) {
			if ev.Stage == "upload" {
				lines = append(lines, ev.Detail)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	if m.uploads != 0 {
		t.Errorf("uploaded %d", m.uploads)
	}
	if len(lines) != 1 || lines[0] != "img1: no local copy; using hv1's template (vmtpl-000000000001)" {
		t.Errorf("progress lines %q", lines)
	}
}

// With no template either (or a path that is not a thin LV) the not-found
// message is today's, word for word.
func TestSyncImageNotFoundMessageUnchanged(t *testing.T) {
	t.Chdir(t.TempDir())
	cache := filepath.Join(t.TempDir(), vmtool.ImageManifestFile)
	empty := t.TempDir()
	want := ""
	for i, path := range []string{"", "/pool/img1", "/dev/vg-thin/../x"} {
		svc := &Service{Images: LocalImages{Dir: empty, Pools: notFound}, ImageCachePath: cache}
		m := &tplVMs{tplPath: path}
		_, err := stubSvc(t, svc).CreateVM(context.Background(), m, CreateVMInput{Host: "hv1", Name: "web", Image: "img1"})
		if !vmtool.IsNotFound(err) || m.uploads != 0 {
			t.Fatalf("path %q: %v, %d uploads", path, err, m.uploads)
		}
		if i == 0 {
			want = err.Error()
			for _, s := range []string{"img1", "image_dir (" + empty + ")", "hv1"} {
				if !strings.Contains(want, s) {
					t.Errorf("message %q lacks %q", want, s)
				}
			}
		} else if err.Error() != want {
			t.Errorf("path %q: message changed:\n got %q\nwant %q", path, err.Error(), want)
		}
	}
	// a host with the image cached never asks ImagePath here
	m := &tplVMs{tplPath: "/dev/vg-thin/vmtpl-000000000001"}
	m.remote = RemoteImage{SHA256: "ab", Size: 9}
	if _, err := stubSvc(t, &Service{Images: LocalImages{Dir: empty, Pools: notFound}, ImageCachePath: cache}).
		CreateVM(context.Background(), m, CreateVMInput{Host: "hv1", Name: "web", Image: "img1"}); err != nil {
		t.Fatal(err)
	}
	if m.asked != 0 {
		t.Errorf("TemplatePath asked %d times for an image the host has", m.asked)
	}
}
