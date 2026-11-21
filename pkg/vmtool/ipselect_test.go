package vmtool

import (
	"testing"
	"time"
)

func TestPickIP(t *testing.T) {
	t0 := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	older, newer := "52:54:00:aa:00:01", "52:54:00:bb:00:02"
	foreign := "52:54:00:ff:ff:ff"
	arp := func(mac, ip string) IfaceAddr { return IfaceAddr{MAC: mac, IPs: []string{ip}} }

	tests := []struct {
		name string
		macs []string
		src  IPSources
		want string
	}{
		{
			name: "commissioning case: .44 leased to newer MAC, stale ARP for both (newer VM)",
			macs: []string{newer},
			src: IPSources{
				Leases: []LeaseRow{{MAC: newer, IP: "192.168.122.44", Expiry: t0}},
				ARP:    []IfaceAddr{arp(older, "192.168.122.44"), arp(newer, "192.168.122.44")},
			},
			want: "192.168.122.44",
		},
		{
			name: "commissioning case: older VM gets nothing",
			macs: []string{older},
			src: IPSources{
				Leases: []LeaseRow{{MAC: newer, IP: "192.168.122.44", Expiry: t0}},
				ARP:    []IfaceAddr{arp(older, "192.168.122.44"), arp(newer, "192.168.122.44")},
			},
			want: "",
		},
		{
			name: "agent for contested address ignored too",
			macs: []string{older},
			src: IPSources{
				Leases: []LeaseRow{{MAC: newer, IP: "192.168.122.44", Expiry: t0}},
				Agent:  []IfaceAddr{arp(older, "192.168.122.44")},
			},
			want: "",
		},
		{
			name: "two rows for one MAC: newest expiry wins",
			macs: []string{newer},
			src: IPSources{Leases: []LeaseRow{
				{MAC: newer, IP: "192.168.122.44", Expiry: t0},
				{MAC: newer, IP: "192.168.122.126", Expiry: t0.Add(time.Hour)},
			}},
			want: "192.168.122.126",
		},
		{
			name: "two rows for one MAC, newest listed first",
			macs: []string{newer},
			src: IPSources{Leases: []LeaseRow{
				{MAC: newer, IP: "192.168.122.126", Expiry: t0.Add(time.Hour)},
				{MAC: newer, IP: "192.168.122.44", Expiry: t0},
			}},
			want: "192.168.122.126",
		},
		{
			name: "agent result for a foreign MAC ignored",
			macs: []string{newer},
			src:  IPSources{Agent: []IfaceAddr{arp(foreign, "192.168.122.9")}},
			want: "",
		},
		{
			name: "no lease, ARP only: ARP address",
			macs: []string{newer},
			src:  IPSources{ARP: []IfaceAddr{arp(foreign, "192.168.122.8"), arp(newer, "192.168.122.7")}},
			want: "192.168.122.7",
		},
		{
			name: "lease wins over differing agent address",
			macs: []string{newer},
			src: IPSources{
				Leases: []LeaseRow{{MAC: newer, IP: "192.168.122.50", Expiry: t0}},
				Agent:  []IfaceAddr{arp(newer, "192.168.122.60")},
				ARP:    []IfaceAddr{arp(newer, "192.168.122.70")},
			},
			want: "192.168.122.50",
		},
		{
			name: "no lease: agent wins over ARP",
			macs: []string{newer},
			src: IPSources{
				Agent: []IfaceAddr{arp(newer, "192.168.122.60")},
				ARP:   []IfaceAddr{arp(newer, "192.168.122.70")},
			},
			want: "192.168.122.60",
		},
		{
			name: "MAC comparison is case-insensitive",
			macs: []string{"52:54:00:BB:00:02"},
			src: IPSources{
				Leases: []LeaseRow{{MAC: newer, IP: "192.168.122.50", Expiry: t0}},
			},
			want: "192.168.122.50",
		},
		{
			// V2: VMInfo.ip is the first card's address. A later card (a vmtool
			// network, whose reservation never expires and whose lease may be
			// newer) never takes it over while the first card has one.
			name: "two cards with leases: the first card wins, whatever the expiry",
			macs: []string{older, newer},
			src: IPSources{Leases: []LeaseRow{
				{MAC: older, IP: "192.168.122.10", Expiry: t0},
				{MAC: newer, IP: "10.77.1.100", Expiry: t0.Add(time.Hour)},
			}},
			want: "192.168.122.10",
		},
		{
			name: "two cards with leases, rows listed second card first: still the first card",
			macs: []string{older, newer},
			src: IPSources{Leases: []LeaseRow{
				{MAC: newer, IP: "10.77.1.100", Expiry: t0.Add(time.Hour)},
				{MAC: older, IP: "192.168.122.10", Expiry: t0},
			}},
			want: "192.168.122.10",
		},
		{
			name: "the first card has no lease yet: the second card's lease is used",
			macs: []string{older, newer},
			src: IPSources{Leases: []LeaseRow{
				{MAC: newer, IP: "10.77.1.100", Expiry: t0},
			}},
			want: "10.77.1.100",
		},
		{
			name: "two cards, agent only: the first card's address, whatever the order reported",
			macs: []string{older, newer},
			src: IPSources{Agent: []IfaceAddr{
				{MAC: newer, IPs: []string{"10.77.1.100"}},
				{MAC: older, IPs: []string{"192.168.122.10"}},
			}},
			want: "192.168.122.10",
		},
		{
			name: "first card's lease beats the second card's agent address",
			macs: []string{older, newer},
			src: IPSources{
				Leases: []LeaseRow{{MAC: older, IP: "192.168.122.10", Expiry: t0}},
				Agent:  []IfaceAddr{{MAC: newer, IPs: []string{"10.77.1.100"}}},
			},
			want: "192.168.122.10",
		},
		{
			name: "loopback and IPv6 ignored",
			macs: []string{newer},
			src:  IPSources{Agent: []IfaceAddr{{MAC: newer, IPs: []string{"127.0.0.1", "fe80::1", "192.168.122.3"}}}},
			want: "192.168.122.3",
		},
		{
			name: "no sources: nothing",
			macs: []string{newer},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pickIP(tc.macs, tc.src)
			if got != tc.want {
				t.Errorf("pickIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseDomainNICs(t *testing.T) {
	xml := `<domain><devices>
<interface type='network'><mac address='52:54:00:AA:00:01'/><source network='default'/></interface>
<interface type='bridge'><mac address='52:54:00:bb:00:02'/><source bridge='br0'/></interface>
</devices></domain>`
	macs, nets, err := parseDomainNICs(xml)
	if err != nil {
		t.Fatal(err)
	}
	if len(macs) != 2 || macs[0] != "52:54:00:aa:00:01" || macs[1] != "52:54:00:bb:00:02" {
		t.Errorf("macs = %v", macs)
	}
	if len(nets) != 1 || nets[0] != "default" {
		t.Errorf("nets = %v", nets)
	}
}
