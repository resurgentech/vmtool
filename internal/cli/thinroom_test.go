package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

const cliRoomMsg = "thin pool thin-test (vg-test) has no room: data use is 80.00%, at or above the 80% limit"

// noRoomMgr is a host whose thin pool is full, as a daemon's 409 reaches the
// workstation (a conflict that matches vmtool.ErrThinNoRoom).
type noRoomMgr struct{ cliFake }

func (m noRoomMgr) refusal() error {
	return &app.Error{Kind: app.KindConflict, Err: &vmtool.ThinRoomError{Msg: cliRoomMsg}}
}
func (m noRoomMgr) ResizeDisk(string, uint) error { return m.refusal() }
func (m noRoomMgr) AddDisk(string, uint, string) (*vmtool.DiskDevice, error) {
	return nil, m.refusal()
}

// add-disk and resize-disk on a full thin pool fail with the daemon's message,
// numbers included, as an ordinary error: the same exit path as any conflict
// (no remoteExitError, so exit status 1).
func TestDiskCommandsNoRoomPrintTheNumbers(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	t.Chdir(t.TempDir())
	dir := writeWorkdir(t, "name: hv1\n"+goodConfig(), "")
	f := installFakeHosts(t)
	old := newResolver
	newResolver = func(e *env) *hosts.Resolver {
		return hosts.NewWith(e.cfg.Name, config.RoleWorkstation, nil, func() (app.Manager, func(), error) {
			return noRoomMgr{cliFake{host: e.cfg.Name, f: f}}, func() {}, nil
		})
	}
	t.Cleanup(func() { newResolver = old })
	for _, args := range [][]string{
		{"add-disk", "hv1/vm1", "50"},
		{"resize-disk", "hv1/vm1", "50"},
	} {
		r := runGate(t, dir, map[string]bool{"vmtool " + args[0]: true}, args...)
		var rx remoteExitError
		if r.err == nil || !errors.Is(r.err, vmtool.ErrThinNoRoom) || !strings.Contains(r.err.Error(), "80.00%") || errors.As(r.err, &rx) {
			t.Errorf("%s: err = %v, want the pool's message with the numbers as an ordinary error", args[0], r.err)
		}
	}
}
