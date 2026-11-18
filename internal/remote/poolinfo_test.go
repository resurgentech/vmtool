package remote

import (
	"net/http"
	"strings"
	"testing"
)

// The client maps pool type and disk backing from a 2.2.0 daemon.
func TestRemoteMapsTypeAndBacking(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/pools") {
			_, _ = w.Write([]byte(`[{"name":"nvme","path":"/dev/vg-test","active":true,"type":"logical"}]`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"vm1","state":"running","vcpus":1,"memory":1,"ip":"","autostart":false,"disk_backing":"block"}`))
	})
	m := f.manager(t)
	pools, err := m.ListPools()
	if err != nil || len(pools) != 1 || pools[0].Type != "logical" {
		t.Fatalf("ListPools = %+v, %v", pools, err)
	}
	info, err := m.Info("vm1")
	if err != nil || info.DiskBacking != "block" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
}

// A 2.1.0 daemon answers without the fields: they decode to "".
func TestRemoteOldDaemonLacksTypeAndBacking(t *testing.T) {
	f := newFakeAPI(t, "2.1.0", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/pools") {
			_, _ = w.Write([]byte(`[{"name":"vmtool","path":"/srv/vmtool","active":true}]`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"vm1","state":"running","vcpus":1,"memory":1,"ip":"","autostart":false}`))
	})
	m := f.manager(t)
	pools, err := m.ListPools()
	if err != nil || len(pools) != 1 || pools[0].Type != "" || pools[0].Name != "vmtool" {
		t.Fatalf("ListPools = %+v, %v", pools, err)
	}
	info, err := m.Info("vm1")
	if err != nil || info.DiskBacking != "" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
}
