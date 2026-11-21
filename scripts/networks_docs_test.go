package scripts_test

import (
	"os"
	"strings"
	"testing"
)

// V7: the docs for tranche 17 (VM networks) name each topic: one-host and
// cross-host networks, the home, names and fixed addresses, what happens when
// the home or a member is down, the unencrypted overlay, MTU 1450, the manual
// repair after a network restart, bridge numbering with and without a store,
// reuse of numbers and subnets, and how `networks` prints. The spec README adds
// that REST clients order the attach primitives themselves. The CLI help is
// pinned through the root command's source files. Whitespace is collapsed, so
// a re-wrap of the prose does not break the test. Reads text files only.

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestNetworksDocsKeywords(t *testing.T) {
	for _, f := range []struct {
		path string
		keys []string
	}{
		{"../README.md", []string{
			"### VM networks",
			"#### A network on one host",
			"#### A network that spans hosts",
			"#### Names and fixed addresses",
			"#### When the home or a member is down",
			"#### After a network restart",
			"vmtool net create hv1 net1",
			"vmtool net attach hv1/web net1",
			"vmtool net join hv2 net1",
			"vmtool net leave hv2 net1",
			"net1 (vmtool)",
			"net1 (vmtool, member)",
			"vmt-000001",
			"vmt-002711",
			"10.77.1.0/24",
			"MTU is 1450",
			"1 to 9999",
			"10000 and up",
			"are handed out again",
			"neither authenticated nor encrypted",
			"anyone on the LAN can read",
			"VM-to-VM traffic by address keeps working",
			"never expire",
			"takes the peer list from the registry when it wakes",
			"restart libvirtd",
			"not seen until the vmtool daemon restarts",
			"Deleting a registered network",
			"vm.net1.test",
			"UDP 4789",
			"vms: false",
			"fixed address survives",
		}},
		{"../spec/README.md", []string{
			"REST clients order the attach primitives themselves",
			"`createReservation`, then `attachInterface` with its MAC",
			"`detachInterface`, then `deleteReservation`",
			"Deleting a registered network",
			"not seen until the vmtool daemon restarts",
			"never expire",
			"MTU 1450",
			"1 to 9999",
			"10000",
			"`name (vmtool)`",
			"`name (vmtool, member)`",
			"neither authenticated nor encrypted",
		}},
	} {
		b, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		text := collapse(string(b))
		for _, k := range f.keys {
			if !strings.Contains(text, collapse(k)) {
				t.Errorf("%s lacks %q", f.path, k)
			}
		}
	}
}

// The CLI help names what an operator needs before reading the README.
func TestNetworksCLIHelpKeywords(t *testing.T) {
	b, err := os.ReadFile("../internal/cli/network.go")
	if err != nil {
		t.Fatal(err)
	}
	text := collapse(string(b))
	for _, k := range []string{
		"MTU 1450",
		"neither authenticated nor encrypted",
		"1 to 9999",
		"from 10000",
		"never expire",
		"removes the network on every member and its registry entry",
		"vmtool networks",
		"name (vmtool, member)",
		"restart libvirtd",
		"README",
	} {
		if !strings.Contains(text, k) {
			t.Errorf("internal/cli/network.go help lacks %q", k)
		}
	}
}
