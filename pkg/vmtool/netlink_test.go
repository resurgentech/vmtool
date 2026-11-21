package vmtool

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestNetHelperClientArgv(t *testing.T) {
	const helper = "/usr/local/libexec/vmtool-net"
	nh := &NetHelper{}
	for _, c := range []struct {
		name string
		run  func() error
		want []string
	}{
		{"link", func() error {
			return nh.Link(10001, "vmt-002711", "192.0.2.11", "eth-test", []string{"192.0.2.12", "192.0.2.13"})
		}, []string{"-n", helper, "link", "10001", "vmt-002711", "192.0.2.11", "eth-test", "192.0.2.12", "192.0.2.13"}},
		{"link no peers", func() error {
			return nh.Link(10001, "vmt-002711", "192.0.2.11", "eth-test", nil)
		}, []string{"-n", helper, "link", "10001", "vmt-002711", "192.0.2.11", "eth-test"}},
		{"unlink", func() error { return nh.Unlink(10001) }, []string{"-n", helper, "unlink", "10001"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := thinStub(t)
			if err := c.run(); err != nil {
				t.Fatal(err)
			}
			if got := readCall(t, log); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argv = %q\nwant   %q", got, c.want)
			}
		})
	}
}

func TestNetHelperCustomPath(t *testing.T) {
	log := thinStub(t)
	if err := (&NetHelper{Helper: "/opt/test/vmtool-net"}).Unlink(7); err != nil {
		t.Fatal(err)
	}
	if got, want := readCall(t, log), []string{"-n", "/opt/test/vmtool-net", "unlink", "7"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

// readCallOrNone: the stub sudo's log, or an error when it never ran.
func readCallOrNone(log string) ([]byte, error) { return os.ReadFile(log) }

// The client refuses what the helper would, before it runs sudo at all.
func TestNetHelperClientRefusals(t *testing.T) {
	nh := &NetHelper{}
	peers := []string{"192.0.2.12"}
	for name, run := range map[string]func() error{
		"vni zero":        func() error { return nh.Link(0, "vmt-000000", "192.0.2.11", "eth-test", peers) },
		"vni too big":     func() error { return nh.Link(1<<24, "vmt-000000", "192.0.2.11", "eth-test", peers) },
		"unlink vni zero": func() error { return nh.Unlink(0) },
		"bridge not vmt":  func() error { return nh.Link(10001, "virbr0", "192.0.2.11", "eth-test", peers) },
		"bridge mismatch": func() error { return nh.Link(10001, "vmt-000001", "192.0.2.11", "eth-test", peers) },
		"local ipv6":      func() error { return nh.Link(10001, "vmt-002711", "2001:db8::1", "eth-test", peers) },
		"local hostname":  func() error { return nh.Link(10001, "vmt-002711", "hv1", "eth-test", peers) },
		"peer ipv6":       func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "eth-test", []string{"2001:db8::2"}) },
		"peer is local":   func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "eth-test", []string{"192.0.2.11"}) },
		"peer option":     func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "eth-test", []string{"--dst"}) },
		"dev lo":          func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "lo", peers) },
		"dev vmt":         func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "vmt-002711", peers) },
		"dev vx":          func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "vx10002", peers) },
		"dev virbr":       func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "virbr0", peers) },
		"dev vnet":        func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "vnet3", peers) },
		"dev option":      func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "-x", peers) },
		"dev empty":       func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "", peers) },
		"dev path":        func() error { return nh.Link(10001, "vmt-002711", "192.0.2.11", "../eth", peers) },
	} {
		t.Run(name, func(t *testing.T) {
			log := thinStub(t)
			if err := run(); err == nil {
				t.Fatal("no error")
			}
			if _, err := readCallOrNone(log); err == nil {
				t.Error("sudo ran")
			}
		})
	}
}

// A refusal by the helper comes back as one line with the verb.
func TestNetHelperErrorLine(t *testing.T) {
	thinStub(t)
	t.Setenv("STUB_RC", "1")
	t.Setenv("STUB_ERR", "vmtool-net: bridge does not exist")
	err := (&NetHelper{}).Link(10001, "vmt-002711", "192.0.2.11", "eth-test", nil)
	if err == nil || err.Error() != "vmtool-net link: bridge does not exist" {
		t.Fatalf("err = %v", err)
	}
}

const netReportFixture = `{"links":[
 {"ifname":"vx10002","master":"vmt-002712","link":"eth-test","linkinfo":{"info_kind":"vxlan","info_data":{"id":10002,"local":"192.0.2.11","port":4789}}},
 {"ifname":"vx10001","master":"vmt-002711","linkinfo":{"info_kind":"vxlan","info_data":{"id":10001,"link":"eth-test","local":"192.0.2.11","port":4789}}},
 {"ifname":"vxlan0","master":"br0","link":"eth-test","linkinfo":{"info_kind":"vxlan","info_data":{"id":5,"local":"192.0.2.11"}}},
 {"ifname":"vx10003","linkinfo":{"info_kind":"vxlan","info_data":{"id":10009,"local":"192.0.2.11"}}}
],"fdb":[
 {"mac":"00:00:00:00:00:00","dev":"vx10001","dst":"192.0.2.13","state":"permanent"},
 {"mac":"00:00:00:00:00:00","dev":"vx10001","dst":"192.0.2.12","state":"permanent"},
 {"mac":"52:54:00:aa:bb:cc","dev":"vx10001","dst":"192.0.2.12"},
 {"mac":"00:00:00:00:00:00","dev":"vx10002","dst":"192.0.2.12"},
 {"mac":"00:00:00:00:00:00","dev":"eth-test","dst":"192.0.2.99"},
 {"mac":"52:54:00:aa:bb:cc","dev":"vnet1","master":"vmt-002711"}
]}`

func TestParseNetReport(t *testing.T) {
	got, err := ParseNetReport([]byte(netReportFixture))
	if err != nil {
		t.Fatal(err)
	}
	want := []NetLink{
		{VNI: 10001, Device: "vx10001", Bridge: "vmt-002711", Local: "192.0.2.11", Dev: "eth-test", Peers: []string{"192.0.2.12", "192.0.2.13"}},
		{VNI: 10002, Device: "vx10002", Bridge: "vmt-002712", Local: "192.0.2.11", Dev: "eth-test", Peers: []string{"192.0.2.12"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %+v\nwant    %+v", got, want)
	}
}

func TestParseNetReportEmptyAndBad(t *testing.T) {
	got, err := ParseNetReport([]byte(`{"links":[],"fdb":[]}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	if _, err := ParseNetReport([]byte(`{"links":[`)); err == nil {
		t.Error("truncated JSON accepted")
	}
}

func TestNetHelperReport(t *testing.T) {
	log := thinStub(t)
	t.Setenv("STUB_OUT", netReportFixture)
	got, err := (&NetHelper{}).Report()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].VNI != 10001 || !strings.HasPrefix(got[0].Device, "vx") {
		t.Fatalf("links = %+v", got)
	}
	if c, want := readCall(t, log), []string{"-n", "/usr/local/libexec/vmtool-net", "report"}; !reflect.DeepEqual(c, want) {
		t.Fatalf("argv = %q, want %q", c, want)
	}
}
