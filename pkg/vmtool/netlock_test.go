package vmtool

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jaredeh/vmtool/internal/testexec"
)

// Z1: link and unlink of one network are serialised in the process. A stub
// sudo keeps state: link records the device, and a later report shows it. The
// report sleeps, to widen the check-then-act race.
func netLockStub(t *testing.T) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	state := filepath.Join(dir, "state")
	testexec.Stub(t, map[string]string{"sudo": `#!/bin/sh
verb=$3
echo "$verb" >> '` + calls + `'
case "$verb" in
report)
  /bin/sleep 0.3
  if [ -f '` + state + `' ]; then cat '` + state + `'; else printf '{"links":[],"fdb":[]}'; fi ;;
link)
  /bin/sleep 0.05
  [ -f '` + state + `' ] && { echo "RTNETLINK answers: File exists" >&2; exit 1; }
  printf '%s' '{"links":[{"ifname":"vx10001","master":"vmt-002711","link":"eth0","linkinfo":{"info_kind":"vxlan","info_data":{"id":10001,"local":"192.0.2.11"}}}],"fdb":[{"mac":"00:00:00:00:00:00","dev":"vx10001","dst":"192.0.2.12"}]}' > '` + state + `' ;;
unlink)
  rm -f '` + state + `' ;;
esac
`})
	return calls
}

func TestEnsureLinkLockedSerialises(t *testing.T) {
	calls := netLockStub(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = (&NetHelper{}).EnsureLinkLocked(10001, "vmt-002711", "192.0.2.11", "eth0", []string{"192.0.2.12"})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	b, _ := os.ReadFile(calls)
	if got := strings.Fields(string(b)); strings.Join(got, ",") != "report,link,report" {
		t.Errorf("helper calls %v, want report,link,report (one after the other)", got)
	}
}

func TestUnlinkLockedWaitsForLink(t *testing.T) {
	calls := netLockStub(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = (&NetHelper{}).EnsureLinkLocked(10001, "vmt-002711", "192.0.2.11", "eth0", []string{"192.0.2.12"})
	}()
	go func() {
		defer wg.Done()
		_ = (&NetHelper{}).UnlinkLocked(10001)
	}()
	wg.Wait()
	b, _ := os.ReadFile(calls)
	s := strings.Join(strings.Fields(string(b)), ",")
	if s != "report,link,unlink" && s != "unlink,report,link" {
		t.Errorf("helper calls %s: link and unlink interleaved", s)
	}
}
