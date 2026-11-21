package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jaredeh/vmtool/internal/app"
	"github.com/jaredeh/vmtool/internal/config"
)

// V5: `vmtool net join|leave <host> <name>`. They need the store (a network
// that spans hosts is registered there); the CLI has none in these tests, so it
// is in local mode and refuses, before any host is opened. What a join and a
// leave do is internal/app's, tested there.

var spanKeep = map[string]bool{"vmtool net join": true, "vmtool net leave": true}

func TestNetJoinAndLeaveNeedTheStore(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	f := installFakeHosts(t)
	for _, args := range [][]string{
		{"net", "join", "veritech01", "net1"},
		{"net", "join", "veritech01", "net1", "--home", "veritech02"},
		{"net", "leave", "veritech01", "net1"},
	} {
		r := runGate(t, dir, spanKeep, args...)
		if r.err == nil || !strings.Contains(r.err.Error(), "needs the store") {
			t.Errorf("%v: %v, want a refusal naming the store", args, r.err)
		}
	}
	if got := f.netSeen(); len(got) != 0 {
		t.Errorf("a host was asked: %q", got)
	}
}

func TestNetJoinHelp(t *testing.T) {
	t.Setenv(config.TokenEnv, gateToken)
	dir := writeWorkdir(t, "name: jetfire\n"+goodConfig(), "")
	r := runGate(t, dir, spanKeep, "net", "join", "--help")
	for _, want := range []string{"--home", "4789", "store", "bridge number", "recreate"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("join help lacks %q:\n%s", want, r.out)
		}
	}
	r = runGate(t, dir, spanKeep, "net", "leave", "--help")
	for _, want := range []string{"home", "VMs"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("leave help lacks %q:\n%s", want, r.out)
		}
	}
	r = runGate(t, dir, spanKeep, "net", "--help")
	for _, want := range []string{"join", "leave"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("net help lacks %q", want)
		}
	}
}

func TestWriteJoinAndLeaveResults(t *testing.T) {
	var b bytes.Buffer
	writeJoin(&b, &app.JoinResult{Network: "net1", Host: "hv1", Home: "home1", VNI: 10000, LanIP: "192.0.2.11", LanDev: "eth-test",
		Registered: true, Peers: []string{"192.0.2.12", "192.0.2.13"}, Skipped: []string{"hv2 was not told the new peer list (timed out); it takes it from the registry when it wakes"}})
	for _, want := range []string{`Host hv1 joined network "net1"`, "VNI 10000", "home home1", "192.0.2.11", "eth-test", "192.0.2.12, 192.0.2.13",
		`registered`, "hv2 was not told"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("join output lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	writeJoin(&b, &app.JoinResult{Network: "net1", Host: "hv1", Home: "home1", VNI: 10000, LanIP: "192.0.2.11", LanDev: "eth-test"})
	if strings.Contains(b.String(), "registered") || !strings.Contains(b.String(), "no other host") {
		t.Errorf("join output with no peers or registration:\n%s", b.String())
	}
	b.Reset()
	writeLeave(&b, &app.LeaveResult{Network: "net1", Host: "hv1", Skipped: []string{"hv2 was not told"}})
	if !strings.Contains(b.String(), `Host hv1 left network "net1"`) || !strings.Contains(b.String(), "hv2 was not told") {
		t.Errorf("leave output:\n%s", b.String())
	}
}
