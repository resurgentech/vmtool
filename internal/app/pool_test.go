package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// poolVMs records the pool CreateVM and DeleteImage hand the Manager.
type poolVMs struct {
	createVMs
	pools []string
}

func (f *poolVMs) CloneImage(base, name, pool string) (string, error) {
	f.pools = append(f.pools, "clone "+pool)
	return "/pool/" + name + ".qcow2", nil
}
func (f *poolVMs) CreateVolume(pool, name string, gb uint) (string, error) {
	f.pools = append(f.pools, "volume "+pool)
	return "/pool/" + name, nil
}
func (f *poolVMs) DeleteImage(name, pool string) error {
	f.pools = append(f.pools, "delete "+pool)
	return nil
}

// app never fills in a pool: an empty one reaches the Manager, which (or
// whose host) knows its default.
func TestCreateVMPassesEmptyPool(t *testing.T) {
	for _, tc := range []struct{ pool, extra, want string }{
		{"", "", "clone ,volume "},
		{"tp", "", "clone tp,volume tp"},
		{"", "fast", "clone ,volume fast"},
	} {
		t.Chdir(t.TempDir())
		m := &poolVMs{createVMs: createVMs{ip: "10.0.0.5"}}
		if _, err := (&Service{}).CreateVM(context.Background(), m, CreateVMInput{
			Host: "jetfire", Name: "web", Image: "u.qcow2",
			Pool: tc.pool, ExtraDiskPool: tc.extra, ExtraDiskSizeGB: 3,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if got := fmt.Sprint(join(m.pools)); got != tc.want {
			t.Errorf("pool %q extra %q: Manager saw %q, want %q", tc.pool, tc.extra, got, tc.want)
		}
	}
}

func TestDeleteImagePassesEmptyPool(t *testing.T) {
	m := &poolVMs{}
	if err := (&Service{}).DeleteImage(context.Background(), m, "u.qcow2", ""); err != nil {
		t.Fatal(err)
	}
	if join(m.pools) != "delete " {
		t.Errorf("Manager saw %q, want the empty pool", m.pools)
	}
}

func TestDefaultConfigPoolEmpty(t *testing.T) {
	if p := vmtool.DefaultConfig("web", "/b").Pool; p != "" {
		t.Errorf("DefaultConfig Pool = %q, want empty (the host's default)", p)
	}
}

func join(s []string) string { return strings.Join(s, ",") }
