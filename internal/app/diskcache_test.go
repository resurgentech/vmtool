package app

import (
	"context"
	"testing"
)

func createCache(t *testing.T, in CreateVMInput) (*uefiVMs, error) {
	t.Helper()
	t.Chdir(t.TempDir())
	m := &uefiVMs{}
	in.Host, in.Name, in.Image = "jetfire", "web", "u.qcow2"
	_, err := (&Service{}).CreateVM(context.Background(), m, in)
	return m, err
}

// The default is resolved here and sent explicitly to Define.
func TestCreateDiskCacheResolution(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   CreateVMInput
		want string
	}{
		{"clone default is unsafe", CreateVMInput{}, "unsafe"},
		{"clone names none", CreateVMInput{DiskCache: "none"}, "none"},
		{"clone names writeback", CreateVMInput{DiskCache: "writeback"}, "writeback"},
		{"noclone default has no attribute", CreateVMInput{Noclone: true}, ""},
		{"noclone names a mode", CreateVMInput{Noclone: true, DiskCache: "writethrough"}, "writethrough"},
	} {
		m, _ := createCache(t, tc.in)
		if m.defined.DiskCache != tc.want {
			t.Errorf("%s: Define got %q, want %q", tc.name, m.defined.DiskCache, tc.want)
		}
	}
}

func TestCreateInvalidDiskCache(t *testing.T) {
	_, err := createCache(t, CreateVMInput{DiskCache: "directsync"})
	if ae := asAppErr(t, err); ae.Kind != KindInvalid {
		t.Fatalf("kind %v: %v", ae.Kind, err)
	}
}
