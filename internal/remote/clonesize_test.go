package remote

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// CloneImage sends size_bytes only when it is positive, so a clone with no
// size asked for is the same request an older daemon already understands.
func TestRemoteCloneSendsSizeOnlyWhenPositive(t *testing.T) {
	for _, tc := range []struct {
		size    uint64
		present bool
		want    string
	}{
		{0, false, ""},
		{5 << 30, true, `"size_bytes":5368709120`},
	} {
		var body string
		f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/volumes/clone") {
				b, _ := io.ReadAll(r.Body)
				body = string(b)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"path":"/dev/vg-test/web"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		m := f.manager(t)
		if _, err := m.CloneImage("/pool/base.qcow2", "web", "nvme", tc.size); err != nil {
			t.Fatalf("size %d: %v", tc.size, err)
		}
		if has := strings.Contains(body, "size_bytes"); has != tc.present {
			t.Errorf("size %d: body %s, size_bytes present = %v, want %v", tc.size, body, has, tc.present)
		}
		if tc.present && !strings.Contains(body, tc.want) {
			t.Errorf("size %d: body %s lacks %s", tc.size, body, tc.want)
		}
	}
}
