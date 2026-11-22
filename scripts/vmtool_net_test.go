package scripts_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The helper deploy/vmtool-net runs as root on a hypervisor. These tests run
// a COPY of it with bash, in which the PATH line is replaced by a directory
// of stubs (ip and bridge, recording their argv into one shared log, plus
// symlinks to bash, env and cat) and the sysfs line by a temp directory laid
// out like /sys/class/net. The installed script has no environment override
// at all. No real ip or bridge ever runs here.

const (
	netScript  = "../deploy/vmtool-net"
	netPathTok = "PATH=/usr/sbin:/usr/bin:/sbin:/bin"
	netSysTok  = "sysnet=/sys/class/net"
	netLockTok = "lockfile=/run/lock/vmtool-net.lock"
)

// netReportLinks and netReportFDB are what the stub ip and bridge print for
// report (ip -j -d link show type vxlan; bridge -j fdb show).
const (
	netReportLinks = `[{"ifname":"vx10001","master":"vmt-002711"}]`
	netReportFDB   = `[{"mac":"00:00:00:00:00:00","dev":"vx10001","dst":"192.0.2.12"}]`
)

type netEnv struct {
	t      *testing.T
	stub   string
	sys    string
	script string
}

func newNetEnv(t *testing.T) *netEnv {
	t.Helper()
	src, err := os.ReadFile(netScript)
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, tok := range []string{netPathTok, netSysTok, netLockTok} {
		if n := strings.Count(s, tok); n != 1 {
			t.Fatalf("script has %d occurrences of %q, want exactly 1", n, tok)
		}
	}
	stub := thinTemp(t)
	// Both stubs append to one log, so the order across them is asserted. A
	// line in fail.txt that the call's argv contains makes that call fail.
	body := `echo "$(basename "$0") $*" >> "$d/calls"
if [[ -f $d/fail.txt ]] && grep -qF -- "$(cat "$d/fail.txt")" <<<"$*"; then exit 1; fi`
	// show.txt and fdbdev.txt are what ip -d -o link show dev vx10001 and
	// bridge fdb show dev vx10001 print for a device that already exists.
	addStub(t, stub, "ip", body+"\n"+`[[ "$*" == "-j -d link show type vxlan" ]] && cat "$d/links.json"
[[ "$*" == "-d -o link show dev vx10001" && -f $d/show.txt ]] && cat "$d/show.txt"; true`)
	addStub(t, stub, "bridge", body+"\n"+`[[ "$*" == "-j fdb show" ]] && cat "$d/fdb.json"
[[ "$*" == "fdb show dev vx10001" && -f $d/fdbdev.txt ]] && cat "$d/fdbdev.txt"; true`)
	// flock records its call in its own log; flockfail.txt makes it fail.
	addStub(t, stub, "flock", `echo "flock $*" >> "$d/flock.log"
[[ -f $d/flockfail.txt ]] && exit 1; true`)
	for _, tool := range []string{"bash", "env", "dirname", "cat", "basename", "grep"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(stub, tool)); err != nil {
			t.Fatal(err)
		}
	}
	sys := thinTemp(t)
	for _, d := range []string{"eth-test", "lo", "vmt-002711/bridge", "vmt-000001/bridge", "plain", "virbr0", "vnet3", "vx10002"} {
		if err := os.MkdirAll(filepath.Join(sys, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s = strings.Replace(s, netPathTok, "PATH="+stub, 1)
	s = strings.Replace(s, netSysTok, "sysnet="+sys, 1)
	s = strings.Replace(s, netLockTok, "lockfile="+filepath.Join(thinTemp(t), "lock"), 1)
	script := filepath.Join(thinTemp(t), "vmtool-net")
	if err := os.WriteFile(script, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &netEnv{t: t, stub: stub, sys: sys, script: script}
	e.write("links.json", netReportLinks)
	e.write("fdb.json", netReportFDB)
	return e
}

func (e *netEnv) write(name, body string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.stub, name), []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *netEnv) run(args ...string) result {
	e.t.Helper()
	cmd := exec.Command("bash", append([]string{e.script}, args...)...)
	cmd.Dir = thinTemp(e.t)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + cmd.Dir}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return result{so.String(), se.String(), code}
}

func (e *netEnv) calls() string { return readOr(e.t, filepath.Join(e.stub, "calls")) }

// refuses: non-zero exit, exactly one stderr line with the helper's prefix,
// no stdout, and neither ip nor bridge was run.
func (e *netEnv) refuses(args ...string) {
	e.t.Helper()
	r := e.run(args...)
	if r.code == 0 {
		e.t.Errorf("%v: exit 0, want a refusal: %q", args, r.stdout)
	}
	line := strings.TrimSuffix(r.stderr, "\n")
	if line == "" || strings.Contains(line, "\n") || !strings.HasPrefix(line, "vmtool-net: ") {
		e.t.Errorf("%v: want one stderr line prefixed %q, got %q", args, "vmtool-net: ", r.stderr)
	}
	if r.stdout != "" {
		e.t.Errorf("%v: stdout on a refusal: %q", args, r.stdout)
	}
	if c := e.calls(); c != "" {
		e.t.Errorf("%v: ip or bridge ran: %q", args, c)
	}
}

func (e *netEnv) ok(args ...string) result {
	e.t.Helper()
	r := e.run(args...)
	if r.code != 0 || r.stderr != "" {
		e.t.Fatalf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	return r
}

func (e *netEnv) wantCalls(want string) {
	e.t.Helper()
	if got := e.calls(); got != want {
		e.t.Errorf("calls:\n got %q\nwant %q", got, want)
	}
}

// VNI 10001 is 0x2711; the bridge is vmt-002711.
const (
	netVNI    = "10001"
	netBridge = "vmt-002711"
)

func TestNetLinkArgv(t *testing.T) {
	e := newNetEnv(t)
	r := e.ok("link", netVNI, netBridge, "192.0.2.11", "eth-test", "192.0.2.12", "192.0.2.13")
	if r.stdout != "" {
		t.Errorf("stdout %q", r.stdout)
	}
	e.wantCalls("ip link add vx10001 type vxlan id 10001 dstport 4789 local 192.0.2.11 dev eth-test\n" +
		"ip link set vx10001 master vmt-002711\n" +
		"bridge fdb append 00:00:00:00:00:00 dev vx10001 dst 192.0.2.12\n" +
		"bridge fdb append 00:00:00:00:00:00 dev vx10001 dst 192.0.2.13\n" +
		"ip link set vx10001 up\n")
}

// No peers is allowed: the device exists and floods nowhere.
func TestNetLinkNoPeers(t *testing.T) {
	e := newNetEnv(t)
	e.ok("link", netVNI, netBridge, "192.0.2.11", "eth-test")
	e.wantCalls("ip link add vx10001 type vxlan id 10001 dstport 4789 local 192.0.2.11 dev eth-test\n" +
		"ip link set vx10001 master vmt-002711\n" +
		"ip link set vx10001 up\n")
}

// A device already there is replaced, so the flood entries are exactly the
// given ones; a repeated peer is added once.
func TestNetLinkReplaces(t *testing.T) {
	e := newNetEnv(t)
	if err := os.MkdirAll(filepath.Join(e.sys, "vx10001"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.ok("link", netVNI, netBridge, "192.0.2.11", "eth-test", "192.0.2.12", "192.0.2.12")
	e.wantCalls("ip -d -o link show dev vx10001\n" +
		"ip link del vx10001\n" +
		"ip link add vx10001 type vxlan id 10001 dstport 4789 local 192.0.2.11 dev eth-test\n" +
		"ip link set vx10001 master vmt-002711\n" +
		"bridge fdb append 00:00:00:00:00:00 dev vx10001 dst 192.0.2.12\n" +
		"ip link set vx10001 up\n")
}

// A step that fails after the device was made removes the device again and
// the helper fails with one line.
func TestNetLinkCleansUpOnFailure(t *testing.T) {
	e := newNetEnv(t)
	e.write("fail.txt", "fdb append")
	r := e.run("link", netVNI, netBridge, "192.0.2.11", "eth-test", "192.0.2.12")
	if r.code == 0 {
		t.Fatalf("exit 0")
	}
	if line := strings.TrimSuffix(r.stderr, "\n"); !strings.HasPrefix(line, "vmtool-net: ") || strings.Contains(line, "\n") {
		t.Errorf("stderr %q", r.stderr)
	}
	got := e.calls()
	if !strings.HasSuffix(got, "bridge fdb append 00:00:00:00:00:00 dev vx10001 dst 192.0.2.12\nip link del vx10001\n") {
		t.Errorf("calls %q: want the device deleted after the failure", got)
	}
}

func TestNetUnlink(t *testing.T) {
	e := newNetEnv(t)
	if err := os.MkdirAll(filepath.Join(e.sys, "vx10001"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.ok("unlink", netVNI)
	e.wantCalls("ip link del vx10001\n")
}

// A device that is not there is already unlinked: success, nothing run.
func TestNetUnlinkAbsent(t *testing.T) {
	e := newNetEnv(t)
	e.ok("unlink", netVNI)
	e.wantCalls("")
}

func TestNetReport(t *testing.T) {
	e := newNetEnv(t)
	r := e.ok("report")
	want := `{"links":` + netReportLinks + `,"fdb":` + netReportFDB + "}\n"
	if r.stdout != want {
		t.Errorf("stdout %q, want %q", r.stdout, want)
	}
	e.wantCalls("ip -j -d link show type vxlan\nbridge -j fdb show\n")
}

func TestNetRefusals(t *testing.T) {
	ok := []string{"link", netVNI, netBridge, "192.0.2.11", "eth-test", "192.0.2.12"}
	with := func(i int, v string) []string {
		a := append([]string{}, ok...)
		a[i] = v
		return a
	}
	cases := map[string][]string{
		"no verb":              {},
		"unknown verb":         {"flush"},
		"verb with options":    {"--help"},
		"report with argument": {"report", "x"},
		"unlink no vni":        {"unlink"},
		"unlink extra":         {"unlink", netVNI, "x"},
		"unlink bad vni":       {"unlink", "abc"},
		"link too few args":    {"link", netVNI, netBridge, "192.0.2.11"},
		"vni zero":             with(1, "0"),
		"vni too big":          with(1, "16777216"),
		"vni leading zero":     with(1, "010001"),
		"vni negative":         with(1, "-1"),
		"vni hex":              with(1, "0x2711"),
		"vni empty":            with(1, ""),
		"vni option":           with(1, "--force"),
		"vni not matching":     with(1, "10002"),
		"bridge not vmt":       with(2, "virbr0"),
		"bridge missing":       with(2, "vmt-00abcd"),
		"bridge not a bridge":  with(2, "vmt-fffffe"),
		"bridge uppercase":     with(2, "vmt-00271A"),
		"bridge short":         with(2, "vmt-2711"),
		"bridge path":          with(2, "../vmt-002711"),
		"bridge option":        with(2, "-d"),
		"local ipv6":           with(3, "2001:db8::1"),
		"local malformed":      with(3, "192.0.2"),
		"local octet":          with(3, "192.0.2.256"),
		"local leading zero":   with(3, "192.0.02.11"),
		"local hostname":       with(3, "hv1"),
		"local option":         with(3, "-4"),
		"local any":            with(3, "0.0.0.0"),
		"local loopback":       with(3, "127.0.0.1"),
		"local multicast":      with(3, "224.0.0.1"),
		"dev missing":          with(4, "nope0"),
		"dev lo":               with(4, "lo"),
		"dev vmt":              with(4, "vmt-002711"),
		"dev vx":               with(4, "vx10002"),
		"dev virbr":            with(4, "virbr0"),
		"dev vnet":             with(4, "vnet3"),
		"dev option":           with(4, "-x"),
		"dev path":             with(4, "../eth-test"),
		"dev too long":         with(4, "abcdefghijklmnop"),
		"peer ipv6":            with(5, "2001:db8::2"),
		"peer malformed":       with(5, "192.0.2.1.5"),
		"peer hostname":        with(5, "hv2"),
		"peer option":          with(5, "--dst"),
		"peer empty":           with(5, ""),
		"peer any":             with(5, "0.0.0.0"),
		"peer is local":        with(5, "192.0.2.11"),
		"peer with a space":    with(5, "192.0.2.12 dev x"),
		"second peer bad":      append(append([]string{}, ok...), "2001:db8::3"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			e := newNetEnv(t)
			e.refuses(args...)
		})
	}
}
