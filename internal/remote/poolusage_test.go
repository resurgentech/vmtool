package remote

import (
	"net/http"
	"strings"
	"testing"
)

// A 2.4.0 daemon's usage fields map across; a 2.3.0 daemon's response without
// them decodes to unknown (nil), not to 0.
func TestRemoteMapsPoolUsage(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/pools") {
			_, _ = w.Write([]byte(`[{"name":"nvme-thin","path":"/dev/vg-test","active":true,"type":"thin",` +
				`"size_bytes":64424509440,"used_bytes":16106127360,"virtual_bytes":32212254720,` +
				`"data_percent":25,"metadata_percent":1.5},` +
				`{"name":"old","path":"/srv/old","active":true}]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	pools, err := f.manager(t).ListPools()
	if err != nil || len(pools) != 2 {
		t.Fatalf("ListPools = %+v, %v", pools, err)
	}
	p := pools[0]
	if p.Type != "thin" || p.SizeBytes == nil || *p.SizeBytes != 64424509440 ||
		p.UsedBytes == nil || *p.UsedBytes != 16106127360 ||
		p.VirtualBytes == nil || *p.VirtualBytes != 32212254720 ||
		p.DataPercent == nil || *p.DataPercent != 25 ||
		p.MetaPercent == nil || *p.MetaPercent != 1.5 {
		t.Errorf("thin pool = %+v", p)
	}
	o := pools[1]
	if o.SizeBytes != nil || o.UsedBytes != nil || o.VirtualBytes != nil || o.DataPercent != nil || o.MetaPercent != nil {
		t.Errorf("old pool's missing fields must be unknown, got %+v", o)
	}
}
