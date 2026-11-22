package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/config"
	"github.com/resurgentech/vmtool/internal/hosts"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

func i64(n int64) *int64     { return &n }
func f64(f float64) *float64 { return &f }

type usagePools struct{ *rtMgr }

func (usagePools) ListPools() ([]vmtool.PoolInfo, error) {
	return []vmtool.PoolInfo{
		{Name: "nvme-thin", Path: "/dev/vg-test", Active: true, Type: "thin",
			SizeBytes: i64(64424509440), UsedBytes: i64(16106127360), VirtualBytes: i64(32212254720),
			DataPercent: f64(25), MetaPercent: f64(1.5)},
		{Name: "vmtool", Path: "/srv/vmtool", Active: true, Type: "dir", SizeBytes: i64(100), UsedBytes: i64(0)},
		{Name: "old", Path: "/srv/old"},
	}, nil
}

// GET pool list carries the usage fields; unknown ones are omitted, and a
// known zero is kept.
func TestListPoolsCarriesUsage(t *testing.T) {
	fm := usagePools{newRTMgr(t)}
	res := hosts.NewWith("box", config.RoleHypervisor, nil, func() (app.Manager, func(), error) { return fm, func() {}, nil })
	rec := do(handlerFor(&handlers{hosts: res, role: config.RoleHypervisor}), http.MethodGet, "/v1/hosts/box/pools", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET pools: %d %s", rec.Code, rec.Body)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	th := got[0]
	if th["type"] != "thin" || th["size_bytes"] != float64(64424509440) || th["used_bytes"] != float64(16106127360) ||
		th["virtual_bytes"] != float64(32212254720) || th["data_percent"] != float64(25) || th["metadata_percent"] != 1.5 {
		t.Errorf("thin pool = %v", th)
	}
	if v, ok := got[1]["used_bytes"]; !ok || v != float64(0) {
		t.Errorf("a known zero must be sent: %v", got[1])
	}
	if _, ok := got[1]["virtual_bytes"]; ok {
		t.Errorf("unknown virtual_bytes must be omitted: %v", got[1])
	}
	for _, k := range []string{"size_bytes", "used_bytes", "virtual_bytes", "data_percent", "metadata_percent"} {
		if _, ok := got[2][k]; ok {
			t.Errorf("unknown %s must be omitted: %v", k, got[2])
		}
	}
}
