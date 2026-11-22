package remote

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/pkg/vmtool"
)

// A 409 from addDisk or resize-volume (a thin pool without room) is a
// conflict that matches vmtool.ErrThinNoRoom, with the daemon's text.
func TestRemoteDiskNoRoomMatchesErrThinNoRoom(t *testing.T) {
	const msg = "thin pool thin-test (vg-test) has no room: data use is 80.00%, at or above the 80% limit"
	for name, call := range map[string]func(m *Manager) error{
		"addDisk":    func(m *Manager) error { _, err := m.AddDisk("vm1", 50, "thin-test"); return err },
		"resizeDisk": func(m *Manager) error { return m.ResizeDisk("vm1", 50) },
	} {
		f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
			jsonReply(w, 409, map[string]string{"error": msg})
		})
		err := call(f.manager(t))
		if last := f.paths[len(f.paths)-1]; !strings.HasSuffix(last, map[string]string{"addDisk": "/add-disk", "resizeDisk": "/resize-volume"}[name]) {
			t.Errorf("%s: asked %q", name, last)
		}
		var ae *app.Error
		if !errors.Is(err, vmtool.ErrThinNoRoom) || !app.AsError(err, &ae) || ae.Kind != app.KindConflict || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: err = %v, want a conflict matching ErrThinNoRoom with %q", name, err, msg)
		}
	}
	// any other status stays what it was
	f := newFakeAPI(t, specVersion(t), func(w http.ResponseWriter, r *http.Request) {
		jsonReply(w, 500, map[string]string{"error": "boom"})
	})
	if _, err := f.manager(t).AddDisk("vm1", 50, ""); errors.Is(err, vmtool.ErrThinNoRoom) {
		t.Errorf("a 500 matched ErrThinNoRoom: %v", err)
	}
}
