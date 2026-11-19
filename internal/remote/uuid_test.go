package remote

import (
	"net/http"
	"testing"
)

// The client reads uuid from a 2.3.0 daemon.
func TestRemoteMapsUUID(t *testing.T) {
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"vm1","state":"running","vcpus":1,"memory":1,"ip":"","autostart":false,"uuid":"00000000-0000-4000-8000-000000000001"}`))
	})
	info, err := f.manager(t).Info("vm1")
	if err != nil || info.UUID != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
}

// An older daemon omits uuid: it is "" (unknown), not an error.
func TestRemoteOldDaemonLacksUUID(t *testing.T) {
	f := newFakeAPI(t, "2.2.0", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"vm1","state":"running","vcpus":1,"memory":1,"ip":"","autostart":false}`))
	})
	info, err := f.manager(t).Info("vm1")
	if err != nil || info.UUID != "" {
		t.Fatalf("Info = %+v, %v", info, err)
	}
}
