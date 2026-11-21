package server

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
	"github.com/jaredeh/vmtool/internal/hosts"
	"github.com/jaredeh/vmtool/internal/testexec"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// Expiry of export volumes: once at start and on every tick, on a hypervisor
// only, with the clock injected. The helper is a stub sudo.
func TestStartExportExpiry(t *testing.T) {
	log := t.TempDir() + "/sudo.log"
	report := `{"report":[{"lv":[` +
		`{"lv_name":"thin-test","vg_name":"vg-test","lv_attr":"twi-aotz--","lv_size":"64424509440","pool_lv":"","origin":"","data_percent":"1.00","metadata_percent":"1.00","lv_tags":"vmtool_thinpool","lv_time":""},` +
		`{"lv_name":"vmexp-000000000001","vg_name":"vg-test","lv_attr":"-wi-a-----","lv_size":"1073741824","pool_lv":"","origin":"","data_percent":"","metadata_percent":"","lv_tags":"vmtool_export,vmtool_name=proj,vmtool_from=vmtpl-000000000001,vmtool_bytes=10","lv_time":"2026-11-20 12:00:00 +0000"}` +
		`]}]}`
	testexec.Stub(t, map[string]string{"sudo": "#!/bin/sh\necho \"$*\" >> '" + log + "'\nprintf '%s' \"$STUB_OUT\"\n"})
	t.Setenv("STUB_OUT", report)

	mk := func(role string) *hosts.Resolver {
		return hosts.NewWith("box", role, nil, func() (app.Manager, func(), error) { return &fakeMgr{}, func() {}, nil }).
			WithThin(&vmtool.Thin{Helper: "/usr/local/libexec/vmtool-thin"})
	}
	clock := []time.Time{
		time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC), // at start: 1 h old
		time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC), // first tick: 25 h
		time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC),
	}
	next := 0
	now := func() time.Time { c := clock[next]; next++; return c }

	// a workstation has no thin pools: nothing runs, the done channel is closed
	ticks := make(chan time.Time)
	stop := make(chan struct{})
	<-startExportExpiry(config.RoleWorkstation, mk(config.RoleWorkstation), now, ticks, stop)
	if _, err := os.Stat(log); err == nil {
		t.Fatal("a workstation ran the helper")
	}

	done := startExportExpiry(config.RoleHypervisor, mk(config.RoleHypervisor), now, ticks, stop)
	ticks <- time.Time{}
	ticks <- time.Time{} // taken only after the first tick's run ended
	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the expiry loop did not stop")
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	const rep = "-n /usr/local/libexec/vmtool-thin report"
	const rm = "-n /usr/local/libexec/vmtool-thin remove vg-test vmexp-000000000001"
	// start: report only (too young); tick 1 and tick 2 (the second may be cut
	// short by the stop, but its report is read after the clock is taken)
	if len(got) < 4 || got[0] != rep || got[1] != rep || got[2] != rm || got[3] != rep {
		t.Errorf("helper calls = %q", got)
	}
	if next < 3 {
		t.Errorf("the clock was read %d times, want 3", next)
	}
}
