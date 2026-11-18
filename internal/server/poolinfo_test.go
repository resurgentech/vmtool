package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// typedPools answers ListPools with a dir and a logical pool.
type typedPools struct{ *rtMgr }

func (typedPools) ListPools() ([]vmtool.PoolInfo, error) {
	return []vmtool.PoolInfo{
		{Name: "vmtool", Path: "/srv/vmtool", Active: true, Type: "dir"},
		{Name: "nvme", Path: "/dev/vg-test", Active: true, Type: "logical"},
		{Name: "old", Path: "/srv/old"},
	}, nil
}

// GET pool list carries each pool's type; an unknown type is left out.
func TestListPoolsCarriesType(t *testing.T) {
	fm := typedPools{newRTMgr(t)}
	res := hosts.NewWith("box", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return fm, func() {}, nil })
	rec := do(handlerFor(&handlers{hosts: res, role: config.RoleHypervisor}), http.MethodGet, "/v1/hosts/box/pools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET pools: %d %s", rec.Code, rec.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0]["type"] != "dir" || got[1]["type"] != "logical" {
		t.Errorf("pools = %v", got)
	}
	if _, ok := got[2]["type"]; ok {
		t.Errorf("unknown type must be omitted: %v", got[2])
	}
}

func TestToAPIDiskBacking(t *testing.T) {
	if v := toAPI(&vmtool.VMInfo{Name: "a", DiskBacking: "block"}); v.DiskBacking == nil || string(*v.DiskBacking) != "block" {
		t.Errorf("block: %+v", v.DiskBacking)
	}
	if v := toAPI(&vmtool.VMInfo{Name: "a", DiskBacking: "file"}); v.DiskBacking == nil || string(*v.DiskBacking) != "file" {
		t.Errorf("file: %+v", v.DiskBacking)
	}
	if v := toAPI(&vmtool.VMInfo{Name: "a"}); v.DiskBacking != nil {
		t.Errorf("unset disk backing must be omitted, got %q", *v.DiskBacking)
	}
	b, _ := json.Marshal(toAPI(&vmtool.VMInfo{Name: "a"}))
	if strings.Contains(string(b), "disk_backing") {
		t.Errorf("JSON carries disk_backing when unset: %s", b)
	}
}
