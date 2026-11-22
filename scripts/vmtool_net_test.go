package scripts_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	netScript   = "../deploy/vmtool-net"
	netPathTok  = "PATH=/usr/sbin:/usr/bin:/sbin:/bin"
	netSysTok   = "sysnet=/sys/class/net"
	netLockTok  = "lockfile=/run/vmtool-net.lock"
	netPauseTok = "retry_pause=0.2"
)

// What the stub ip and bridge print for report. The name listing (ip -j link
// show type vxlan, no -d) and the fdb line are shaped like real output from
// veritech03 (iproute2 6.1.0); each device's detail comes from ip -j -d link
// show dev <name>, a one-element array. A detailed dump (-d without dev)
// exits 139, as the real ip does whenever a VXLAN device exists.
const (
	netReportNames = `[{"ifindex":19,"ifname":"vx10001","flags":["BROADCAST","MULTICAST"],"mtu":1450,"qdisc":"noop","operstate":"DOWN","linkmode":"DEFAULT","group":"default","txqlen":1000,"link_type":"ether","address":"f6:2e:14:3d:d7:89","broadcast":"ff:ff:ff:ff:ff:ff"}]`
	netDev10001    = `{"ifindex":19,"ifname":"vx10001","master":"vmt-002711","linkinfo":{"info_kind":"vxlan","info_data":{"id":10001,"local":"192.0.2.11","link":"eth-test","port":4789}}}`
	netDev10003    = `{"ifindex":23,"ifname":"vx10003","master":"vmt-002713","linkinfo":{"info_kind":"vxlan","info_data":{"id":10003,"local":"192.0.2.11","link":"eth-test","port":4789}}}`
	netReportLinks = `[` + netDev10001 + `]`
	netReportFDB   = `[{"mac":"00:00:00:00:00:00","ifname":"vx10001","dst":"192.0.2.12","flags":["self"],"state":"permanent"}]`
)

type netEnv struct {
	t      *testing.T
	stub   string
	sys    string
	script string
	lock   string
}

func newNetEnv(t *testing.T) *netEnv {
	t.Helper()
	src, err := os.ReadFile(netScript)
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, tok := range []string{netPathTok, netSysTok, netLockTok, netPauseTok} {
		if n := strings.Count(s, tok); n != 1 {
			t.Fatalf("script has %d occurrences of %q, want exactly 1", n, tok)
		}
	}
	stub := thinTemp(t)
	// Both stubs append to one log, so the order across them is asserted. A
	// line in fail.txt that the call's argv contains makes that call fail.
	body := `echo "$(basename "$0") $*" >> "$d/calls"
if [[ -f $d/fail.txt ]] && grep -qF -- "$(cat "$d/fail.txt")" <<<"$*"; then exit 1; fi
# n.txt: the first n calls of a listing exit 139 (a crash), then it works.
# crash-ip.txt counts the name listing, crash-dev.txt the per-device query.
c=
[[ "$*" == "-j link show type vxlan" || "$*" == "-j fdb show" ]] && c=$d/crash-$(basename "$0").txt
[[ "$*" == "-j -d link show dev "* ]] && c=$d/crash-dev.txt
if [[ -n $c && -f $c ]]; then
  n=$(cat "$c")
  if (( n > 0 )); then echo $((n-1)) > "$c"; exit 139; fi
fi`
	// show.txt and fdbdev.txt are what ip -d -o link show dev vx10001 and
	// bridge fdb show dev vx10001 print for a device that already exists.
	// names.json is the name listing; dev-<name>.json what ip -j -d link show
	// dev <name> prints (no file: the device does not exist). Any detailed
	// dump crashes (139), like iproute2 6.1 with a VXLAN device present.
	addStub(t, stub, "ip", body+"\n"+`a=" $* "
if [[ $a == *" -d "* && $a == *" show "* && $a != *" show dev "* ]]; then exit 139; fi
[[ "$*" == "-j link show type vxlan" ]] && cat "$d/names.json"
if [[ "$*" == "-j -d link show dev "* ]]; then
  n=${!#}
  if [[ -f $d/dev-$n.json ]]; then cat "$d/dev-$n.json"; else echo "Device \"$n\" does not exist." >&2; exit 1; fi
fi
[[ "$*" == "-d -o link show dev vx10001" && -f $d/show.txt ]] && cat "$d/show.txt"; true`)
	addStub(t, stub, "bridge", body+"\n"+`[[ "$*" == "-j fdb show" ]] && cat "$d/fdb.json"
[[ "$*" == "fdb show dev vx10001" && -f $d/fdbdev.txt ]] && cat "$d/fdbdev.txt"; true`)
	// flock records its call in its own log; flockfail.txt makes it fail.
	addStub(t, stub, "flock", `echo "flock $*" >> "$d/flock.log"
[[ -f $d/flockfail.txt ]] && exit 1; true`)
	for _, tool := range []string{"bash", "env", "dirname", "cat", "basename", "grep", "sleep"} {
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
	lock := filepath.Join(thinTemp(t), "lock")
	s = strings.Replace(s, netLockTok, "lockfile="+lock, 1)
	s = strings.Replace(s, netPauseTok, "retry_pause=0", 1)
	script := filepath.Join(thinTemp(t), "vmtool-net")
	if err := os.WriteFile(script, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &netEnv{t: t, stub: stub, sys: sys, script: script, lock: lock}
	e.write("names.json", netReportNames)
	e.write("dev-vx10001.json", "["+netDev10001+"]\n")
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

// wantReport: stdout is exactly the report of links and fdb, and well-formed
// JSON.
func wantReport(t *testing.T, stdout, links, fdb string) {
	t.Helper()
	want := `{"links":` + links + `,"fdb":` + fdb + "}\n"
	if stdout != want {
		t.Errorf("stdout %q\nwant   %q", stdout, want)
	}
	if !json.Valid([]byte(stdout)) {
		t.Errorf("stdout is not well-formed JSON: %q", stdout)
	}
}

// Z3: report lists the names without -d, then asks each device on its own;
// the detailed dump (which crashes the real ip) is never run.
func TestNetReport(t *testing.T) {
	e := newNetEnv(t)
	r := e.ok("report")
	wantReport(t, r.stdout, netReportLinks, netReportFDB)
	e.wantCalls("ip -j link show type vxlan\nip -j -d link show dev vx10001\nbridge -j fdb show\n")
}

// Two devices: their objects joined into one array, in listing order.
func TestNetReportTwoDevices(t *testing.T) {
	e := newNetEnv(t)
	e.write("names.json", `[{"ifindex":19,"ifname":"vx10001","mtu":1450},{"ifindex":23,"ifname":"vx10003","mtu":1450}]`)
	e.write("dev-vx10003.json", "["+netDev10003+"]\n")
	r := e.ok("report")
	wantReport(t, r.stdout, `[`+netDev10001+`,`+netDev10003+`]`, netReportFDB)
	e.wantCalls("ip -j link show type vxlan\nip -j -d link show dev vx10001\nip -j -d link show dev vx10003\nbridge -j fdb show\n")
}

// No VXLAN device: an empty links array.
func TestNetReportNoDevices(t *testing.T) {
	e := newNetEnv(t)
	e.write("names.json", "[]\n")
	r := e.ok("report")
	wantReport(t, r.stdout, `[]`, netReportFDB)
	e.wantCalls("ip -j link show type vxlan\nbridge -j fdb show\n")
}

// A device gone between the listing and its own query is skipped.
func TestNetReportDeviceGone(t *testing.T) {
	e := newNetEnv(t)
	e.write("names.json", `[{"ifname":"vx10005"},{"ifname":"vx10001"}]`)
	r := e.ok("report")
	wantReport(t, r.stdout, netReportLinks, netReportFDB)
	e.wantCalls("ip -j link show type vxlan\nip -j -d link show dev vx10005\nip -j -d link show dev vx10001\nbridge -j fdb show\n")
}

// Only names of the form vx<digits> are ever passed to ip, once each; any
// other name in the listing (other VXLAN devices, or text that is not a name)
// is ignored and the output stays well-formed.
func TestNetReportOnlyVxNames(t *testing.T) {
	e := newNetEnv(t)
	e.write("names.json", `[{"ifname":"vxlan0"},{"ifname":"vx10001"},{"ifname":"-d"},{"ifname":"vx1 type"},`+
		`{"ifname":"x\",\"ifname\":\"vx10003"},{"ifname":"vx10001"},{"ifname":"VX7"},{"ifname":"vx"}]`)
	e.write("dev-vx10003.json", "["+netDev10003+"]\n")
	r := e.ok("report")
	wantReport(t, r.stdout, netReportLinks, netReportFDB)
	e.wantCalls("ip -j link show type vxlan\nip -j -d link show dev vx10001\nbridge -j fdb show\n")
}

// A device that exists but whose query keeps failing is an error, not a
// silent gap; one crash of the query is retried.
func TestNetReportDeviceQueryFails(t *testing.T) {
	e := newNetEnv(t)
	if err := os.MkdirAll(filepath.Join(e.sys, "vx10001"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.write("crash-dev.txt", "1")
	r := e.ok("report")
	wantReport(t, r.stdout, netReportLinks, netReportFDB)

	e = newNetEnv(t)
	if err := os.MkdirAll(filepath.Join(e.sys, "vx10001"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.write("crash-dev.txt", "99")
	r = e.run("report")
	if r.code == 0 || r.stderr != "vmtool-net: cannot list the links\n" || r.stdout != "" {
		t.Errorf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}

// ipDetailRE finds an ip invocation with options (one of them -d, -details or
// a bundle with d) followed by link show and whatever comes after it.
var ipDetailRE = regexp.MustCompile(`\bip((?:\s+-{1,2}[0-9A-Za-z]+)+)\s+link\s+show\b([^\n]*)`)

// detailedDump reports whether line runs ip's detailed link listing in dump
// form (with type, or without dev): the form that crashes iproute2 6.1 when a
// VXLAN device exists. The per-device form (show dev X) is fine.
func detailedDump(line string) bool {
	for _, m := range ipDetailRE.FindAllStringSubmatch(line, -1) {
		detail := false
		for _, f := range strings.Fields(m[1]) {
			if f == "-details" || f == "--details" || (!strings.HasPrefix(f, "--") && strings.Contains(f, "d")) {
				detail = true
			}
		}
		if !detail {
			continue
		}
		rest := strings.TrimSpace(m[2])
		if !strings.HasPrefix(rest, "dev ") || strings.Contains(rest, " type ") {
			return true
		}
	}
	return false
}

func TestDetailedDumpShape(t *testing.T) {
	for line, want := range map[string]bool{
		`list_retry links ip -j -d link show type vxlan || die`: true,
		`ip -d link show`:                       true,
		`ip -j -d link show | x`:                true,
		`ip -details link show type vxlan`:      true,
		`ip -jd link show`:                      true,
		`// ip -j -d link show type vxlan`:      true,
		`show=$(ip -d -o link show dev "$vx")`:  false,
		`one=$(ip -j -d link show dev "$name")`: false,
		`ip -j link show type vxlan`:            false,
		`ip -o link show`:                       false,
		`ip -d link show dev vx1 type vxlan`:    true,
	} {
		if got := detailedDump(line); got != want {
			t.Errorf("%q: %v, want %v", line, got, want)
		}
	}
}

// Z3: no detailed link dump is left in the helper, the deploy scripts or
// pkg/vmtool (iproute2 6.1 segfaults on it whenever a VXLAN device exists).
func TestNoDetailedLinkDump(t *testing.T) {
	var files []string
	for _, root := range []string{"../deploy", "../pkg/vmtool"} {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sh, err := filepath.Glob("*.sh")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, sh...)
	if len(files) < 3 {
		t.Fatalf("scanned only %v", files)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if detailedDump(line) {
				t.Errorf("%s:%d: detailed link dump: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// Output of a query that is not one array of objects is refused, so the
// printed JSON is never malformed.
func TestNetReportBadDeviceOutput(t *testing.T) {
	for _, body := range []string{"", "[]", "garbage", `[{"ifname":"vx10001"}`, `{"ifname":"vx10001"}`} {
		e := newNetEnv(t)
		e.write("dev-vx10001.json", body)
		r := e.run("report")
		if r.code == 0 || r.stderr != "vmtool-net: cannot list the links\n" || r.stdout != "" {
			t.Errorf("%q: exit %d stdout %q stderr %q", body, r.code, r.stdout, r.stderr)
		}
	}
	for _, body := range []string{"", "garbage"} {
		e := newNetEnv(t)
		e.write("fdb.json", body)
		r := e.run("report")
		if r.code == 0 || r.stderr != "vmtool-net: cannot list the flood entries\n" || r.stdout != "" {
			t.Errorf("fdb %q: exit %d stdout %q stderr %q", body, r.code, r.stdout, r.stderr)
		}
	}
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
