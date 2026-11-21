package vmtool

import (
	"encoding/xml"
	"net"
	"strings"
	"time"
)

// IfaceAddr is one guest interface as reported by the agent or ARP source:
// its hardware address and the IPv4/IPv6 addresses seen on it.
type IfaceAddr struct {
	MAC string
	IPs []string
}

// LeaseRow is one row of a libvirt network's DHCP lease table.
type LeaseRow struct {
	MAC    string
	IP     string
	Expiry time.Time
}

// IPSources holds what each address source returned. Leases must be the
// unfiltered rows of every network the domain is attached to, so that an
// address held by another MAC is visible.
type IPSources struct {
	Leases []LeaseRow
	Agent  []IfaceAddr
	ARP    []IfaceAddr
}

func normMAC(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// pickIP chooses the domain's address. Order: lease, agent, ARP. macs are in
// card order and only the FIRST MAC counts: rows and interfaces of a later
// card are never returned (they appear in VMInfo.networks only), even when
// the first card has no address yet. Of several rows for that MAC, the
// newest expiry wins. Leases of every MAC (all attached networks) still
// decide who holds an address: one whose newest row is foreign is unknown and
// is never returned, whatever the agent or ARP say. Returns "" when nothing
// qualifies.
func pickIP(macs []string, src IPSources) string {
	own := map[string]bool{}
	for _, m := range macs {
		own[normMAC(m)] = true
	}

	// Newest row per address decides who holds it; a tie goes to us.
	type holder struct {
		expiry time.Time
		ours   bool
	}
	held := map[string]holder{}
	for _, r := range src.Leases {
		h, ok := held[r.IP]
		ours := own[normMAC(r.MAC)]
		if !ok || r.Expiry.After(h.expiry) || (r.Expiry.Equal(h.expiry) && ours) {
			held[r.IP] = holder{r.Expiry, ours}
		}
	}
	contested := func(ip string) bool {
		h, ok := held[ip]
		return ok && !h.ours
	}

	// Only the first card (the first MAC) counts: a later card, such as one on
	// a vmtool network whose reservation is leased from the start, never
	// stands in while the first card has none yet. Within the MAC the newest
	// expiry wins.
	if len(macs) == 0 {
		return ""
	}
	mac := normMAC(macs[0])
	var best *LeaseRow
	for i := range src.Leases {
		r := &src.Leases[i]
		if normMAC(r.MAC) != mac || contested(r.IP) || !usableIPv4(r.IP) {
			continue
		}
		if best == nil || r.Expiry.After(best.Expiry) {
			best = r
		}
	}
	if best != nil {
		return best.IP
	}

	for _, list := range [][]IfaceAddr{src.Agent, src.ARP} {
		for _, ifc := range list {
			if normMAC(ifc.MAC) != mac {
				continue
			}
			for _, ip := range ifc.IPs {
				if usableIPv4(ip) && !contested(ip) {
					return ip
				}
			}
		}
	}
	return ""
}

func usableIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil && !ip.IsLoopback()
}

// parseDomainNICs returns the lowercase MACs of a domain's interfaces and
// the names of the libvirt networks they attach to (type=network only;
// bridges have no lease table).
func parseDomainNICs(domXML string) (macs, networks []string, err error) {
	var d struct {
		Interfaces []struct {
			Type string `xml:"type,attr"`
			MAC  struct {
				Address string `xml:"address,attr"`
			} `xml:"mac"`
			Source struct {
				Network string `xml:"network,attr"`
			} `xml:"source"`
		} `xml:"devices>interface"`
	}
	if err := xml.Unmarshal([]byte(domXML), &d); err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, i := range d.Interfaces {
		if i.MAC.Address != "" {
			macs = append(macs, normMAC(i.MAC.Address))
		}
		if i.Type == "network" && i.Source.Network != "" && !seen[i.Source.Network] {
			seen[i.Source.Network] = true
			networks = append(networks, i.Source.Network)
		}
	}
	return macs, networks, nil
}
