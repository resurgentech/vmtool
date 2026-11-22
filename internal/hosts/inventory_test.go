package hosts

import (
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/testexec"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

type invMgr struct {
	app.Manager
	disks map[string][]vmtool.VMDisk
}

func (m invMgr) List() ([]vmtool.VMInfo, error) {
	return []vmtool.VMInfo{{Name: "vm1", UUID: "00000000-0000-4000-8000-000000000001"}}, nil
}
func (m invMgr) VMDisks(name string) ([]vmtool.VMDisk, error) { return m.disks[name], nil }

// The reporter's disk list: file disks as libvirt gave them; a block disk that
// is a vmtool thin volume becomes kind thin with the thin pool and size; any
// other block disk stays block.
func TestInventorySourceDisks(t *testing.T) {
	report := `{"report":[{"lv":[` +
		`{"lv_name":"thin-test","vg_name":"vg-test","lv_attr":"twi-aotz--","lv_size":"64424509440","pool_lv":"","origin":"","data_percent":"1.00","metadata_percent":"1.00","lv_tags":"vmtool_thinpool","lv_time":""},` +
		`{"lv_name":"vm1","vg_name":"vg-test","lv_attr":"Vwi-aotz--","lv_size":"10737418240","pool_lv":"thin-test","origin":"","data_percent":"5.00","metadata_percent":"","lv_tags":"vmtool_vol","lv_time":""}` +
		`]}]}`
	testexec.Stub(t, map[string]string{"sudo": "#!/bin/sh\nprintf '%s' \"$STUB_OUT\"\n"})
	t.Setenv("STUB_OUT", report)
	m := invMgr{disks: map[string][]vmtool.VMDisk{"vm1": {
		{Target: "vda", Path: "/dev/vg-test/vm1", Kind: vmtool.DiskBackingBlock},
		{Target: "vdb", Path: "/dev/vg-other/raw", Kind: vmtool.DiskBackingBlock, Pool: "p2", CapacityBytes: 5},
		{Target: "vdc", Path: "/pool/vm1-3.qcow2", Kind: vmtool.DiskBackingFile, Pool: "pool1", CapacityBytes: 7},
	}}}
	r := NewWith("hv1", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return m, func() {}, nil }).
		WithThin(&vmtool.Thin{Helper: "/usr/local/libexec/vmtool-thin"})
	rd, done, err := r.InventorySource().Open()
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if vms, err := rd.List(); err != nil || len(vms) != 1 || vms[0].UUID == "" {
		t.Fatalf("list: %v %v", vms, err)
	}
	got, err := rd.VMDisks("vm1")
	if err != nil {
		t.Fatal(err)
	}
	want := []app.InventoryDisk{
		{Target: "vda", Pool: "thin-test", Kind: "thin", CapacityBytes: 10 << 30},
		{Target: "vdb", Pool: "p2", Kind: "block", CapacityBytes: 5},
		{Target: "vdc", Pool: "pool1", Kind: "file", CapacityBytes: 7},
	}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("disk %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
