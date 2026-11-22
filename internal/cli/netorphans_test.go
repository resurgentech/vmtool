package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/app"
	"github.com/resurgentech/vmtool/internal/storeclient"
)

// G3: `net show` lists the orphaned reservations that remain (the home did not
// answer); with none it prints what it always did.
func TestWriteNetworkShowListsOrphans(t *testing.T) {
	var out bytes.Buffer
	writeNetworkShow(&out, &app.NetworkShow{Info: fakeNet("net1"), Orphans: []storeclient.NetworkReservation{
		{Network: "net1", Mac: "52:54:00:00:00:01", Ip: "10.77.1.100", Host: "hv1", Vm: "web1"},
	}})
	for _, want := range []string{"net1", "orphaned:", "10.77.1.100", "52:54:00:00:00:01", "web1", "hv1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("show lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	writeNetworkShow(&out, &app.NetworkShow{Info: fakeNet("net1")})
	if strings.Contains(out.String(), "orphaned") {
		t.Errorf("a network with no orphans lists some:\n%s", out.String())
	}
}
