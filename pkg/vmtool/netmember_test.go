package vmtool

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/resurgentech/vmtool/internal/testexec"
)

// V5: the member form of a network (the bridge and the MTU, nothing else), the
// underlay lookup (ip route get, no root), and re-linking only what differs.

func TestBuildMemberNetworkXMLGolden(t *testing.T) {
	got, err := BuildMemberNetworkXML("net1", "vmt-002711")
	if err != nil {
		t.Fatal(err)
	}
	want := `<network>
  <name>net1</name>
  <bridge name='vmt-002711' stp='off'/>
  <mtu size='1450'/>
</network>
`
	if got != want {
		t.Fatalf("XML =\n%s\nwant\n%s", got, want)
	}
	for _, bad := range []string{"<metadata", "<forward", "<ip ", "<dhcp", "<domain"} {
		if strings.Contains(got, bad) {
			t.Errorf("member XML has %s", bad)
		}
	}
}

func TestBuildMemberNetworkXMLRefusals(t *testing.T) {
	for name, c := range map[string][2]string{
		"default name": {"default", "vmt-002711"},
		"bad name":     {"net 1", "vmt-002711"},
		"not vmtool":   {"net1", "virbr1"},
		"upper hex":    {"net1", "vmt-00271A"},
	} {
		if _, err := BuildMemberNetworkXML(c[0], c[1]); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParseNetworkXMLMemberForm(t *testing.T) {
	x, _ := BuildMemberNetworkXML("net1", "vmt-002711")
	n, err := ParseNetworkXML(x)
	if err != nil {
		t.Fatal(err)
	}
	if !n.Vmtool || n.VNI != 10001 || n.MTU != NetworkMTU || !n.IsMemberForm() {
		t.Errorf("member = %+v, want vmtool, VNI 10001, MTU 1450, member form", n)
	}
	home, _ := BuildNetworkXML(NetworkSpec{Name: "net1", Bridge: "vmt-002711", Subnet: netip.MustParsePrefix("10.77.1.0/24"), Domain: "net1.test"})
	h, _ := ParseNetworkXML(home)
	if h.IsMemberForm() {
		t.Error("the home form reads as a member")
	}
	d, _ := ParseNetworkXML(`<network><name>default</name><forward mode='nat'/><bridge name='virbr0'/><ip address='192.168.122.1' netmask='255.255.255.0'/></network>`)
	if d.IsMemberForm() {
		t.Error("libvirt's default network reads as a member")
	}
}

func TestParseRouteGet(t *testing.T) {
	for name, c := range map[string]struct {
		in   string
		want Underlay
		bad  bool
	}{
		"via a gateway": {in: "192.0.2.13 via 192.0.2.1 dev eth-test src 192.0.2.11 uid 1000 \n    cache \n", want: Underlay{IP: "192.0.2.11", Dev: "eth-test"}},
		"on link":       {in: "192.0.2.13 dev eth-test src 192.0.2.11 uid 1000\n", want: Underlay{IP: "192.0.2.11", Dev: "eth-test"}},
		"own address":   {in: "local 192.0.2.11 dev lo src 192.0.2.11 uid 1000\n", bad: true},
		"no source":     {in: "192.0.2.13 dev eth-test\n", bad: true},
		"no device":     {in: "192.0.2.13 src 192.0.2.11\n", bad: true},
		"ipv6 source":   {in: "192.0.2.13 dev eth-test src 2001:db8::1\n", bad: true},
		"empty":         {in: "", bad: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRouteGet(c.in)
			if c.bad {
				if err == nil {
					t.Fatalf("accepted: %+v", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %+v, %v, want %+v", got, err, c.want)
			}
		})
	}
}

func ipStub(t *testing.T) (log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), "ip.log")
	testexec.Stub(t, map[string]string{"ip": `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> '` + log + `'; done
printf '%s' "$STUB_OUT"
exit ${STUB_RC:-0}
`})
	return log
}

func TestFindUnderlayRunsIPRouteGet(t *testing.T) {
	log := ipStub(t)
	t.Setenv("STUB_OUT", "192.0.2.13 via 192.0.2.1 dev eth-test src 192.0.2.11 uid 1000\n")
	got, err := FindUnderlay("192.0.2.13")
	if err != nil || got != (Underlay{IP: "192.0.2.11", Dev: "eth-test"}) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if c := readCall(t, log); !reflect.DeepEqual(c, []string{"route", "get", "192.0.2.13"}) {
		t.Fatalf("argv = %q", c)
	}
}

func TestFindUnderlayRefusesWhatIsNotAnAddress(t *testing.T) {
	log := ipStub(t)
	for _, to := range []string{"home1", "2001:db8::1", "--help", "", "192.0.2.13/24", "127.0.0.1"} {
		if _, err := FindUnderlay(to); err == nil {
			t.Errorf("%q accepted", to)
		}
	}
	if _, err := os.ReadFile(log); err == nil {
		t.Error("ip ran for a refused address")
	}
}

func TestFindUnderlayFailure(t *testing.T) {
	ipStub(t)
	t.Setenv("STUB_RC", "2")
	if _, err := FindUnderlay("192.0.2.13"); err == nil {
		t.Fatal("a failing ip gave no error")
	}
}

// EnsureLink asks the helper for its report and links only when the device is
// not already as wanted.
func TestEnsureLinkSkipsWhatIsAlreadyRight(t *testing.T) {
	for name, c := range map[string]struct {
		bridge, local, dev string
		peers              []string
		wantCalls          []string // the verbs the helper ran
	}{
		"same":         {"vmt-002711", "192.0.2.11", "eth-test", []string{"192.0.2.13", "192.0.2.12"}, []string{"report"}},
		"same, dupes":  {"vmt-002711", "192.0.2.11", "eth-test", []string{"192.0.2.12", "192.0.2.13", "192.0.2.12"}, []string{"report"}},
		"new peer":     {"vmt-002711", "192.0.2.11", "eth-test", []string{"192.0.2.12", "192.0.2.13", "192.0.2.14"}, []string{"report", "link"}},
		"fewer peers":  {"vmt-002711", "192.0.2.11", "eth-test", []string{"192.0.2.12"}, []string{"report", "link"}},
		"other local":  {"vmt-002711", "192.0.2.21", "eth-test", []string{"192.0.2.12", "192.0.2.13"}, []string{"report", "link"}},
		"other device": {"vmt-002711", "192.0.2.11", "eth-other", []string{"192.0.2.12", "192.0.2.13"}, []string{"report", "link"}},
	} {
		t.Run(name, func(t *testing.T) {
			log := thinStub(t)
			t.Setenv("STUB_OUT", netReportFixture)
			changed, err := (&NetHelper{}).EnsureLink(10001, c.bridge, c.local, c.dev, c.peers)
			if err != nil {
				t.Fatal(err)
			}
			var verbs []string
			for _, l := range readCallLines(t, log) {
				verbs = append(verbs, l)
			}
			if !reflect.DeepEqual(verbs, c.wantCalls) {
				t.Fatalf("helper ran %q, want %q", verbs, c.wantCalls)
			}
			if changed != (len(c.wantCalls) == 2) {
				t.Errorf("changed = %v", changed)
			}
		})
	}
}

func TestEnsureLinkOtherBridgeOrNoDeviceLinks(t *testing.T) {
	log := thinStub(t)
	t.Setenv("STUB_OUT", `{"links":[],"fdb":[]}`)
	changed, err := (&NetHelper{}).EnsureLink(10001, "vmt-002711", "192.0.2.11", "eth-test", []string{"192.0.2.12"})
	if err != nil || !changed {
		t.Fatalf("changed = %v, %v", changed, err)
	}
	if v := readCallLines(t, log); !reflect.DeepEqual(v, []string{"report", "link"}) {
		t.Fatalf("verbs = %q", v)
	}
}

// readCallLines returns the verb of each sudo call the stub logged: the third
// argument (sudo -n HELPER VERB ...).
func readCallLines(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for _, call := range strings.Split(strings.TrimSpace(string(b)), "---\n") {
		f := strings.Split(strings.TrimSpace(call), "\n")
		if len(f) >= 3 {
			verbs = append(verbs, f[2])
		}
	}
	return verbs
}

func TestPlanMemberNetwork(t *testing.T) {
	existing := []NetworkInfo{
		{Name: "default", Bridge: "virbr0"},
		{Name: "other", Bridge: "vmt-002712", Vmtool: true, VNI: 10002},
	}
	spec, err := PlanMemberNetwork("net1", 10001, existing)
	if err != nil || spec != (MemberSpec{Name: "net1", Bridge: "vmt-002711"}) {
		t.Fatalf("spec = %+v, %v", spec, err)
	}
	for name, c := range map[string]struct {
		name string
		vni  uint32
		want error
	}{
		"vni zero":         {"net1", 0, ErrInvalidNetwork},
		"vni too big":      {"net1", 1 << 24, ErrInvalidNetwork},
		"default":          {"default", 10001, ErrInvalidNetwork},
		"name taken":       {"other", 10001, ErrNetworkExists},
		"bridge taken":     {"net2", 10002, ErrNetworkExists},
		"not vmtool's own": {"default", 10001, ErrInvalidNetwork},
	} {
		if _, err := PlanMemberNetwork(c.name, c.vni, existing); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}
