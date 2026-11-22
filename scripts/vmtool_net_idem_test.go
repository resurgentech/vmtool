package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Z1: link is idempotent and link and the removal verb take an exclusive lock.

// ipShow is one line of ip -d -o link show dev vx10001.
func ipShow(master, local, dev, dstport string) string {
	return "12: vx10001: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1450 qdisc noqueue master " + master +
		" state UNKNOWN mode DEFAULT group default qlen 1000\\    link/ether 6a:11:22:33:44:55 brd ff:ff:ff:ff:ff:ff promiscuity 1 " +
		"\\    vxlan id 10001 local " + local + " dev " + dev + " srcport 0 0 dstport " + dstport + " ttl auto ageing 300 udpcsum noudp6zerocsumtx\n"
}

func (e *netEnv) existing(show, fdb string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Join(e.sys, "vx10001"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.write("show.txt", show)
	e.write("fdbdev.txt", fdb)
}

// A device already as wanted is not deleted or made again: only the flood
// entries change (the extra one goes, the missing one is added), and it is
// brought up. Entries with another MAC are not touched.
func TestNetLinkIdempotent(t *testing.T) {
	e := newNetEnv(t)
	e.existing(ipShow(netBridge, "192.0.2.11", "eth-test", "4789"),
		"00:00:00:00:00:00 dst 192.0.2.12 self permanent\n"+
			"00:00:00:00:00:00 dst 192.0.2.99 self permanent\n"+
			"aa:bb:cc:dd:ee:ff dst 192.0.2.50 self\n")
	e.ok("link", netVNI, netBridge, "192.0.2.11", "eth-test", "192.0.2.12", "192.0.2.13")
	e.wantCalls("ip -d -o link show dev vx10001\n" +
		"bridge fdb show dev vx10001\n" +
		"bridge fdb del 00:00:00:00:00:00 dev vx10001 dst 192.0.2.99\n" +
		"bridge fdb append 00:00:00:00:00:00 dev vx10001 dst 192.0.2.13\n" +
		"ip link set vx10001 up\n")
}

// The same flood entries: nothing changes but the up.
func TestNetLinkIdempotentNoChange(t *testing.T) {
	e := newNetEnv(t)
	e.existing(ipShow(netBridge, "192.0.2.11", "eth-test", "4789"), "00:00:00:00:00:00 dst 192.0.2.12 self permanent\n")
	e.ok("link", netVNI, netBridge, "192.0.2.11", "eth-test", "192.0.2.12")
	e.wantCalls("ip -d -o link show dev vx10001\nbridge fdb show dev vx10001\nip link set vx10001 up\n")
}

// A device with any other setting is replaced as before.
func TestNetLinkDiffersReplaced(t *testing.T) {
	cases := map[string]string{
		"local":   ipShow(netBridge, "192.0.2.77", "eth-test", "4789"),
		"dev":     ipShow(netBridge, "192.0.2.11", "other0", "4789"),
		"bridge":  ipShow("vmt-000001", "192.0.2.11", "eth-test", "4789"),
		"dstport": ipShow(netBridge, "192.0.2.11", "eth-test", "8472"),
		"garbage": "nonsense\n",
	}
	for name, show := range cases {
		t.Run(name, func(t *testing.T) {
			e := newNetEnv(t)
			e.existing(show, "00:00:00:00:00:00 dst 192.0.2.12 self permanent\n")
			e.ok("link", netVNI, netBridge, "192.0.2.11", "eth-test", "192.0.2.12")
			e.wantCalls("ip -d -o link show dev vx10001\n" +
				"ip link del vx10001\n" +
				"ip link add vx10001 type vxlan id 10001 dstport 4789 local 192.0.2.11 dev eth-test\n" +
				"ip link set vx10001 master vmt-002711\n" +
				"bridge fdb append 00:00:00:00:00:00 dev vx10001 dst 192.0.2.12\n" +
				"ip link set vx10001 up\n")
		})
	}
}

// A flood entry read back that is not a usable address is never passed to
// bridge: the helper fails with one line and leaves the device alone.
func TestNetLinkIdempotentBadFDBEntry(t *testing.T) {
	e := newNetEnv(t)
	e.existing(ipShow(netBridge, "192.0.2.11", "eth-test", "4789"), "00:00:00:00:00:00 dst --evil self permanent\n")
	r := e.run("link", netVNI, netBridge, "192.0.2.11", "eth-test", "192.0.2.12")
	if r.code == 0 || !strings.HasPrefix(r.stderr, "vmtool-net: ") {
		t.Fatalf("exit %d stderr %q", r.code, r.stderr)
	}
	e.wantCalls("ip -d -o link show dev vx10001\nbridge fdb show dev vx10001\n")
}

// link and the removal verb take the exclusive lock first; no lock, no ip or
// bridge.
func TestNetLockTaken(t *testing.T) {
	for _, args := range [][]string{
		{"link", netVNI, netBridge, "192.0.2.11", "eth-test", "192.0.2.12"},
		{"unlink", netVNI},
	} {
		e := newNetEnv(t)
		e.ok(args...)
		if got := readOr(t, filepath.Join(e.stub, "flock.log")); got != "flock 9\n" {
			t.Errorf("%v: flock calls %q, want one flock 9", args, got)
		}
		e = newNetEnv(t)
		e.write("flockfail.txt", "x")
		r := e.run(args...)
		if r.code == 0 || !strings.HasPrefix(r.stderr, "vmtool-net: ") {
			t.Errorf("%v: exit %d stderr %q, want a refusal when the lock fails", args, r.code, r.stderr)
		}
		if c := e.calls(); c != "" {
			t.Errorf("%v: ip or bridge ran without the lock: %q", args, c)
		}
	}
}

// report only reads and takes no lock.
func TestNetReportNoLock(t *testing.T) {
	e := newNetEnv(t)
	e.ok("report")
	if got := readOr(t, filepath.Join(e.stub, "flock.log")); got != "" {
		t.Errorf("report took the lock: %q", got)
	}
}
