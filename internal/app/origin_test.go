package app

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

var originNow = time.Date(2026, 11, 21, 4, 5, 6, 0, time.UTC)

func originSvc() *Service { return &Service{Now: func() time.Time { return originNow }} }

// originCreate creates vm1 from img1 on m and returns what Define was given.
func originCreate(t *testing.T, m *uefiVMs, in CreateVMInput) (vmtool.VMConfig, error) {
	t.Helper()
	t.Chdir(t.TempDir())
	in.Host, in.Name, in.Image, in.NoWait = "hv1", "vm1", "img1", true
	_, err := originSvc().CreateVM(context.Background(), m, in)
	return m.defined, err
}

// Create records the origin for each clone kind in the config Define gets.
func TestCreateRecordsOrigin(t *testing.T) {
	for _, c := range []struct {
		name      string
		clonePath string
		in        CreateVMInput
		want      vmtool.VMOrigin
	}{
		{"thin template", "/dev/vg-thin/vm1", CreateVMInput{Pool: "thin-test"},
			vmtool.VMOrigin{Image: "img1", CloneKind: vmtool.CloneKindThin, Pool: "thin-test"}},
		{"thick LV", "/dev/vg-test/vm1", CreateVMInput{Pool: "nvme"},
			vmtool.VMOrigin{Image: "img1", CloneKind: vmtool.CloneKindBlock, Pool: "nvme"}},
		{"qcow2 file", "", CreateVMInput{Pool: "vmtool"},
			vmtool.VMOrigin{Image: "img1", CloneKind: vmtool.CloneKindFile, Pool: "vmtool"}},
		{"file, host default pool found by its path", "", CreateVMInput{},
			vmtool.VMOrigin{Image: "img1", CloneKind: vmtool.CloneKindFile, Pool: "vmtool"}},
		{"noclone", "", CreateVMInput{Noclone: true},
			vmtool.VMOrigin{Image: "img1", CloneKind: vmtool.CloneKindNone}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := &uefiVMs{createVMs: createVMs{pools: thinTestPools, clonePath: c.clonePath}}
			cfg, err := originCreate(t, m, c.in)
			if err != nil {
				t.Fatal(err)
			}
			want := c.want
			want.Created = originNow
			if !reflect.DeepEqual(cfg.Origin, &want) {
				t.Errorf("Define got origin %+v, want %+v", cfg.Origin, &want)
			}
			if cfg.Comment != "" {
				t.Errorf("comment %q with none asked", cfg.Comment)
			}
		})
	}
}

// The image's SHA-256 is recorded when the create knew it: the host's
// manifest after the sync.
type shaVMs struct {
	uefiVMs
	sha string
}

func (f *shaVMs) ImageManifest(string) (RemoteImage, error) {
	return RemoteImage{SHA256: f.sha, Size: 5}, nil
}
func (f *shaVMs) UploadImage(context.Context, string, io.Reader, int64, string, func(int64)) error {
	return fmt.Errorf("no upload expected")
}

func TestCreateRecordsImageSHA(t *testing.T) {
	t.Chdir(t.TempDir())
	sha := strings.Repeat("a", 64)
	m := &shaVMs{sha: sha}
	_, err := originSvc().CreateVM(context.Background(), m, CreateVMInput{Host: "hv1", Name: "vm1", Image: "img1", NoWait: true})
	if err != nil {
		t.Fatal(err)
	}
	if o := m.defined.Origin; o == nil || o.ImageSHA256 != sha || o.Image != "img1" || o.CloneKind != vmtool.CloneKindFile {
		t.Errorf("origin %+v, want the sha %s", o, sha)
	}
}

// A host whose manifest cannot say (no entry) leaves the digest empty.
func TestCreateOriginWithoutSHA(t *testing.T) {
	m := &uefiVMs{}
	cfg, err := originCreate(t, m, CreateVMInput{})
	if err != nil || cfg.Origin == nil || cfg.Origin.ImageSHA256 != "" {
		t.Errorf("origin %+v err %v", cfg.Origin, err)
	}
}

func TestCreateCarriesComment(t *testing.T) {
	m := &uefiVMs{}
	cfg, err := originCreate(t, m, CreateVMInput{Comment: "placeholder comment"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Comment != "placeholder comment" {
		t.Errorf("Define comment %q", cfg.Comment)
	}
}

// A bad comment is refused before anything is made, naming the field.
func TestCreateRefusesBadComment(t *testing.T) {
	for _, c := range []string{strings.Repeat("a", 513), "two\nlines", "tab\there", "bell\a"} {
		m := &templateVMs{uefiVMs: uefiVMs{createVMs: createVMs{pools: thinTestPools}}}
		_, err := originCreate(t, &m.uefiVMs, CreateVMInput{Comment: c})
		ae := asAppErr(t, err)
		if ae.Kind != KindInvalid || !strings.Contains(err.Error(), "comment") {
			t.Errorf("%.12q: err %v", c, err)
		}
		if m.defined.Name != "" || len(m.calls) != 0 {
			t.Errorf("%.12q: something ran", c)
		}
	}
}

// commentVMs records the calls of a comment change.
type commentVMs struct {
	Manager
	calls   []string
	state   vmtool.VMState
	comment string
	err     error
}

func (f *commentVMs) SetComment(n, c string) error {
	f.calls = append(f.calls, fmt.Sprintf("SetComment %s %q", n, c))
	if f.err != nil {
		return f.err
	}
	f.comment = c
	return nil
}
func (f *commentVMs) Info(n string) (*vmtool.VMInfo, error) {
	f.calls = append(f.calls, "Info "+n)
	return &vmtool.VMInfo{Name: n, State: f.state, Comment: f.comment}, nil
}
func (f *commentVMs) Define(vmtool.VMConfig) error {
	f.calls = append(f.calls, "Define")
	return nil
}

// A comment is set on a running and on a shut-off VM with one metadata call
// and no redefine; empty clears.
func TestSetComment(t *testing.T) {
	for _, st := range []vmtool.VMState{vmtool.StateRunning, vmtool.StateShutoff} {
		m := &commentVMs{state: st}
		info, err := (&Service{}).SetComment(context.Background(), m, VMRef{Host: "hv1", Name: "vm1"}, "placeholder comment")
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{`SetComment vm1 "placeholder comment"`, "Info vm1"}; !reflect.DeepEqual(m.calls, want) {
			t.Errorf("%s: calls %q, want %q", st, m.calls, want)
		}
		if info.Comment != "placeholder comment" || info.State != st {
			t.Errorf("%s: info %+v", st, info)
		}
		m.calls = nil
		if info, err = (&Service{}).SetComment(context.Background(), m, VMRef{Host: "hv1", Name: "vm1"}, ""); err != nil || info.Comment != "" {
			t.Fatalf("clear: %+v %v", info, err)
		}
		if m.calls[0] != `SetComment vm1 ""` {
			t.Errorf("clear calls %q", m.calls)
		}
	}
}

func TestSetCommentRefusesBadComment(t *testing.T) {
	m := &commentVMs{}
	_, err := (&Service{}).SetComment(context.Background(), m, VMRef{Host: "hv1", Name: "vm1"}, strings.Repeat("é", 257))
	ae := asAppErr(t, err)
	if ae.Kind != KindInvalid || !strings.Contains(err.Error(), "comment") || len(m.calls) != 0 {
		t.Errorf("err %v calls %q", err, m.calls)
	}
}

func TestSetCommentUnknownVM(t *testing.T) {
	m := &commentVMs{err: fmt.Errorf("looking up domain: %w", vmtool.ErrNotFound)}
	_, err := (&Service{}).SetComment(context.Background(), m, VMRef{Host: "hv1", Name: "ghost"}, "x")
	if asAppErr(t, err).Kind != KindNotFound {
		t.Errorf("err %v", err)
	}
}
