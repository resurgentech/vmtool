package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/pkg/vmtool"
)

var testPools = []vmtool.PoolInfo{
	{Name: "vmtool", Path: "/pool", Type: "dir"},
	{Name: "nvme", Path: "/dev/vg-test", Type: "logical"},
}

func TestCloneCacheDefault(t *testing.T) {
	for _, tc := range []struct {
		name, path, want string
	}{
		{"logical", "/dev/vg-test/vm1", "none"},
		{"logical, path not clean", "/dev/vg-test//vm1", "none"},
		{"dir", "/pool/vm1.qcow2", "unsafe"},
		{"unknown pool", "/elsewhere/vm1.qcow2", "unsafe"},
		{"nested under a logical pool path", "/dev/vg-test/a/vm1", "unsafe"},
	} {
		if got := cloneCacheDefault(testPools, tc.path); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func createLogical(t *testing.T, m *uefiVMs, in CreateVMInput) (*uefiVMs, error) {
	t.Helper()
	t.Chdir(t.TempDir())
	in.Host, in.Name, in.Image = "jetfire", "vm1", "u.qcow2"
	_, err := (&Service{}).CreateVM(context.Background(), m, in)
	return m, err
}

func TestCreateCacheFollowsPoolType(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clone string
		in    CreateVMInput
		want  string
	}{
		{"logical, no cache", "/dev/vg-test/vm1", CreateVMInput{}, "none"},
		{"logical, writeback asked", "/dev/vg-test/vm1", CreateVMInput{DiskCache: "writeback"}, "writeback"},
		{"dir, no cache", "/pool/vm1.qcow2", CreateVMInput{}, "unsafe"},
		{"noclone unchanged", "/dev/vg-test/vm1", CreateVMInput{Noclone: true}, ""},
	} {
		m := &uefiVMs{createVMs: createVMs{pools: testPools, clonePath: tc.clone}}
		m, err := createLogical(t, m, tc.in)
		_ = err
		if m.defined.DiskCache != tc.want {
			t.Errorf("%s: Define got %q, want %q", tc.name, m.defined.DiskCache, tc.want)
		}
	}
}

func TestCreateListPoolsErrorFailsBeforeDefine(t *testing.T) {
	m := &uefiVMs{createVMs: createVMs{poolsErr: errors.New("pools down")}}
	m, err := createLogical(t, m, CreateVMInput{})
	if err == nil || !strings.Contains(err.Error(), "pools down") {
		t.Fatalf("err = %v, want the ListPools error", err)
	}
	if m.defined.Name != "" {
		t.Errorf("Define ran: %+v", m.defined)
	}
}

// An explicit cache never needs the pool list, and noclone never asks.
func TestCreateListPoolsOnlyWhenNeeded(t *testing.T) {
	for _, in := range []CreateVMInput{{DiskCache: "writeback"}, {Noclone: true}} {
		m := &uefiVMs{createVMs: createVMs{poolsErr: errors.New("pools down")}}
		m, err := createLogical(t, m, in)
		if err != nil && strings.Contains(err.Error(), "pools down") {
			t.Errorf("%+v: ListPools was called: %v", in, err)
		}
		if m.defined.Name == "" {
			t.Errorf("%+v: Define did not run: %v", in, err)
		}
	}
}

func TestCreatePassesDiskSizeToClone(t *testing.T) {
	for _, tc := range []struct {
		gb   uint
		want uint64
	}{{0, 0}, {20, 20 << 30}} {
		m := &uefiVMs{}
		m, _ = createLogical(t, m, CreateVMInput{DiskSizeGB: tc.gb})
		if m.cloneSize != tc.want {
			t.Errorf("DiskSizeGB %d: CloneImage got %d, want %d", tc.gb, m.cloneSize, tc.want)
		}
	}
}

func TestCreateCloneEmitNamesRealVolume(t *testing.T) {
	for _, tc := range []struct{ clone, want string }{
		{"/dev/vg-test/vm1", "cloned → vm1"},
		{"/pool/vm1.qcow2", "cloned → vm1.qcow2"},
	} {
		m := &uefiVMs{createVMs: createVMs{pools: testPools, clonePath: tc.clone}}
		var got []string
		t.Chdir(t.TempDir())
		_, _ = (&Service{}).CreateVM(context.Background(), m, CreateVMInput{
			Host: "jetfire", Name: "vm1", Image: "u.qcow2",
			OnProgress: func(ev ProgressEvent) {
				if ev.Stage == "clone" && ev.Status == "done" {
					got = append(got, ev.Detail)
				}
			},
		})
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("clone %s: emitted %q, want %q", tc.clone, got, tc.want)
		}
	}
}
