package vmtool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DefaultNetHelper is where `just deploy` installs the link helper.
const DefaultNetHelper = "/usr/local/libexec/vmtool-net"

// maxVNI is the largest VXLAN network identifier (24 bits).
const maxVNI = 1<<24 - 1

var netDevRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,14}$`)

// NetHelper runs the root-owned link helper deploy/vmtool-net through
// sudo -n. The zero value uses DefaultNetHelper and the sudo on PATH. It
// carries no password and no stdin. The helper repeats every check made here;
// the checks here only save a sudo call for an argument it would refuse.
type NetHelper struct {
	Helper string // helper path; default DefaultNetHelper
}

func (n *NetHelper) helper() string {
	if n == nil || n.Helper == "" {
		return DefaultNetHelper
	}
	return n.Helper
}

func (n *NetHelper) run(verb string, args ...string) ([]byte, error) {
	cmd, err := command("sudo", append([]string{"-n", n.helper(), verb}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("vmtool-net %s: %w", verb, err)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		msg = strings.TrimPrefix(msg, "vmtool-net: ")
		msg = strings.Join(strings.Fields(strings.ReplaceAll(msg, "\n", " ; ")), " ")
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("vmtool-net %s: %s", verb, msg)
	}
	return stdout.Bytes(), nil
}

func netCheckVNI(vni uint32) error {
	if vni < 1 || vni > maxVNI {
		return invalidNetwork("invalid VNI %d", vni)
	}
	return nil
}

// netCheckIPv4 accepts a unicast IPv4 address in dotted-quad form only.
func netCheckIPv4(what, s string) error {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || a.String() != s || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() || a.As4()[0] >= 224 {
		return invalidNetwork("invalid %s %q", what, s)
	}
	return nil
}

func netCheckDev(dev string) error {
	if !netDevRE.MatchString(dev) || dev == "lo" {
		return invalidNetwork("invalid interface %q", dev)
	}
	for _, p := range []string{"vmt-", "vx", "virbr", "vnet"} {
		if strings.HasPrefix(dev, p) {
			return invalidNetwork("interface %q not allowed", dev)
		}
	}
	return nil
}

// Link makes or replaces the VXLAN device vx<vni> on this host, mastered to
// bridge, bound to localIP on dev, flooding to exactly peers. bridge must be
// the vmtool bridge of vni (BridgeName). It is idempotent.
func (n *NetHelper) Link(vni uint32, bridge, localIP, dev string, peers []string) error {
	if err := netCheckVNI(vni); err != nil {
		return err
	}
	if v, ok := BridgeVNI(bridge); !ok || v != vni {
		return invalidNetwork("bridge %q is not the bridge of VNI %d", bridge, vni)
	}
	if err := netCheckIPv4("local address", localIP); err != nil {
		return err
	}
	if err := netCheckDev(dev); err != nil {
		return err
	}
	args := []string{strconv.FormatUint(uint64(vni), 10), bridge, localIP, dev}
	for _, p := range peers {
		if err := netCheckIPv4("peer address", p); err != nil {
			return err
		}
		if p == localIP {
			return invalidNetwork("peer %q is the local address", p)
		}
		args = append(args, p)
	}
	_, err := n.run("link", args...)
	return err
}

// Unlink removes vx<vni>; none there is success.
func (n *NetHelper) Unlink(vni uint32) error {
	if err := netCheckVNI(vni); err != nil {
		return err
	}
	_, err := n.run("unlink", strconv.FormatUint(uint64(vni), 10))
	return err
}

// NetLink is one vx<VNI> device as the helper reports it.
type NetLink struct {
	VNI    uint32
	Device string   // vx<VNI>
	Bridge string   // the device's master
	Local  string   // the underlay address it is bound to
	Dev    string   // the underlay interface
	Peers  []string // its flood entries (all-zero MAC), sorted
}

// Report lists the vx<VNI> devices on this host and their flood entries.
func (n *NetHelper) Report() ([]NetLink, error) {
	out, err := n.run("report")
	if err != nil {
		return nil, err
	}
	return ParseNetReport(out)
}

var netDeviceRE = regexp.MustCompile(`^vx([1-9][0-9]{0,7})$`)

// ParseNetReport reads the helper's report ({"links": ip -j -d link show
// type vxlan, "fdb": bridge -j fdb show}). Only devices named vx<N> whose
// VXLAN id is N are kept, sorted by VNI; a flood entry is one with the
// all-zero MAC on such a device.
func ParseNetReport(b []byte) ([]NetLink, error) {
	var rep struct {
		Links []struct {
			Ifname   string `json:"ifname"`
			Master   string `json:"master"`
			Link     string `json:"link"`
			Linkinfo struct {
				Kind string `json:"info_kind"`
				Data struct {
					ID    uint32 `json:"id"`
					Local string `json:"local"`
					Link  string `json:"link"`
				} `json:"info_data"`
			} `json:"linkinfo"`
		} `json:"links"`
		FDB []struct {
			MAC string `json:"mac"`
			Dev string `json:"dev"`
			Dst string `json:"dst"`
		} `json:"fdb"`
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, fmt.Errorf("vmtool-net report: %w", err)
	}
	var out []NetLink
	idx := map[string]int{}
	for _, l := range rep.Links {
		m := netDeviceRE.FindStringSubmatch(l.Ifname)
		if m == nil || l.Linkinfo.Kind != "vxlan" {
			continue
		}
		id, _ := strconv.ParseUint(m[1], 10, 32)
		if uint32(id) != l.Linkinfo.Data.ID {
			continue
		}
		dev := l.Link
		if dev == "" {
			dev = l.Linkinfo.Data.Link
		}
		idx[l.Ifname] = len(out)
		out = append(out, NetLink{VNI: uint32(id), Device: l.Ifname, Bridge: l.Master, Local: l.Linkinfo.Data.Local, Dev: dev})
	}
	for _, e := range rep.FDB {
		i, ok := idx[e.Dev]
		if !ok || e.MAC != "00:00:00:00:00:00" || e.Dst == "" {
			continue
		}
		out[i].Peers = append(out[i].Peers, e.Dst)
	}
	for i := range out {
		sort.Strings(out[i].Peers)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VNI < out[j].VNI })
	return out, nil
}
