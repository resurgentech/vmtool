package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"
)

// The network registry (tranche 17, V3): the vmtool networks that span hosts,
// their members, and the allocation of their VNIs and subnets. The store only
// records: it never reaches a host and keeps nothing about whether one is up.
// Nothing here is secret; times are RFC3339Nano UTC text.

const (
	// FirstAllocatedVNI is where the allocator starts: bridge vmt-002710. A
	// host with no store numbers its own bridges below it (1..9999).
	FirstAllocatedVNI = 10000
	// MaxVNI is the largest VNI: 24 bits, six hex digits of the bridge name.
	MaxVNI = 1<<24 - 1
)

var (
	// ErrNetworkExists: the name is registered already.
	ErrNetworkExists = errors.New("network name is taken")
	// ErrVNITaken: a given VNI belongs to another network.
	ErrVNITaken = errors.New("vni is taken")
	// ErrSubnetTaken: a given subnet belongs to another network.
	ErrSubnetTaken = errors.New("subnet is taken")
	// ErrNoFreeVNI and ErrNoFreeSubnet: the allocator found nothing free.
	ErrNoFreeVNI    = errors.New("no free vni")
	ErrNoFreeSubnet = errors.New("no free subnet")
	// ErrNetworkHasMembers: a network with members is not deleted.
	ErrNetworkHasMembers = errors.New("network has members")
)

// NetworkMember is one host of a network: its LAN address and the interface
// that address is on (the underlay of its link).
type NetworkMember struct {
	Host   string
	LANIP  string
	LANDev string
	Joined time.Time
}

// Network is a registered network with its members, ordered by host.
type Network struct {
	Name    string
	VNI     uint32
	Subnet  string
	Domain  string
	Home    string
	Created time.Time
	Members []NetworkMember
}

// NetworkNew is a network to register. VNI 0 asks for the lowest free one from
// FirstAllocatedVNI; an empty Subnet asks for the next free /24 of
// 10.77.0.0/16. A given one is used only if free. An empty Domain is
// <name>.test; the caller has validated the strings.
type NetworkNew struct {
	Name, Home, Domain, Subnet string
	VNI                        uint32
}

// CreateNetwork registers n. The checks, the allocation and the insert are one
// transaction on the database's one connection, so concurrent creates never
// share a VNI or a subnet (the UNIQUE columns are the backstop).
func (d *DB) CreateNetwork(ctx context.Context, n NetworkNew) (Network, error) {
	if n.VNI > MaxVNI {
		return Network{}, fmt.Errorf("create network: vni out of range")
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return Network{}, fmt.Errorf("create network: %w", err)
	}
	defer tx.Rollback()
	vnis, subnets := map[uint32]bool{}, map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT name, vni, subnet FROM networks`)
	if err != nil {
		return Network{}, fmt.Errorf("create network: %w", err)
	}
	exists := false
	for rows.Next() {
		var name, subnet string
		var vni uint32
		if err := rows.Scan(&name, &vni, &subnet); err != nil {
			rows.Close()
			return Network{}, fmt.Errorf("create network: %w", err)
		}
		exists = exists || name == n.Name
		vnis[vni], subnets[subnet] = true, true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Network{}, fmt.Errorf("create network: %w", err)
	}
	rows.Close()
	switch {
	case exists:
		return Network{}, ErrNetworkExists
	case n.VNI != 0 && vnis[n.VNI]:
		return Network{}, ErrVNITaken
	case n.Subnet != "" && subnets[n.Subnet]:
		return Network{}, ErrSubnetTaken
	}
	if n.VNI == 0 {
		v := uint32(FirstAllocatedVNI)
		for vnis[v] && v <= MaxVNI {
			v++
		}
		if v > MaxVNI {
			return Network{}, ErrNoFreeVNI
		}
		n.VNI = v
	}
	if n.Subnet == "" {
		for i := 0; i < 256; i++ {
			if c := fmt.Sprintf("10.77.%d.0/24", i); !subnets[c] {
				n.Subnet = c
				break
			}
		}
		if n.Subnet == "" {
			return Network{}, ErrNoFreeSubnet
		}
	}
	if n.Domain == "" {
		n.Domain = n.Name + ".test"
	}
	now := d.now().UTC()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO networks(name, vni, subnet, domain, home, created) VALUES (?, ?, ?, ?, ?, ?)`,
		n.Name, n.VNI, n.Subnet, n.Domain, n.Home, now.Format(time.RFC3339Nano)); err != nil {
		return Network{}, fmt.Errorf("create network: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Network{}, fmt.Errorf("create network: %w", err)
	}
	return Network{Name: n.Name, VNI: n.VNI, Subnet: n.Subnet, Domain: n.Domain, Home: n.Home,
		Created: parseTime(now.Format(time.RFC3339Nano)), Members: []NetworkMember{}}, nil
}

// queryer is what a transaction and the database share.
type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

// loadNetworks reads the networks matching where (with its args), each with
// its members, ordered by name.
func loadNetworks(ctx context.Context, q queryer, where string, args ...any) ([]Network, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name, vni, subnet, domain, home, created FROM networks `+where+` ORDER BY name`, args...)
	if err != nil {
		return nil, err
	}
	var out []Network
	for rows.Next() {
		var n Network
		var created string
		if err := rows.Scan(&n.Name, &n.VNI, &n.Subnet, &n.Domain, &n.Home, &created); err != nil {
			rows.Close()
			return nil, err
		}
		n.Created = parseTime(created)
		n.Members = []NetworkMember{}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for i := range out {
		mr, err := q.QueryContext(ctx,
			`SELECT host, lan_ip, lan_dev, joined FROM network_members WHERE network=? ORDER BY host`, out[i].Name)
		if err != nil {
			return nil, err
		}
		for mr.Next() {
			var m NetworkMember
			var joined string
			if err := mr.Scan(&m.Host, &m.LANIP, &m.LANDev, &joined); err != nil {
				mr.Close()
				return nil, err
			}
			m.Joined = parseTime(joined)
			out[i].Members = append(out[i].Members, m)
		}
		if err := mr.Err(); err != nil {
			mr.Close()
			return nil, err
		}
		mr.Close()
	}
	return out, nil
}

// GetNetwork returns the network named name with its members, or ErrNotFound.
func (d *DB) GetNetwork(ctx context.Context, name string) (Network, error) {
	got, err := loadNetworks(ctx, d.sql, `WHERE name=?`, name)
	if err != nil {
		return Network{}, fmt.Errorf("get network: %w", err)
	}
	if len(got) == 0 {
		return Network{}, ErrNotFound
	}
	return got[0], nil
}

// ListNetworks returns every network with its members, ordered by name.
func (d *DB) ListNetworks(ctx context.Context) ([]Network, error) {
	got, err := loadNetworks(ctx, d.sql, ``)
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	return got, nil
}

// HostNetworks returns the networks host is a member of or the home of, each
// with all its members, ordered by name. A host with none gets an empty slice.
func (d *DB) HostNetworks(ctx context.Context, host string) ([]Network, error) {
	got, err := loadNetworks(ctx, d.sql,
		`WHERE home=? OR name IN (SELECT network FROM network_members WHERE host=?)`, host, host)
	if err != nil {
		return nil, fmt.Errorf("host networks: %w", err)
	}
	if got == nil {
		got = []Network{}
	}
	return got, nil
}

// DeleteNetwork removes a network that has no members: ErrNetworkHasMembers
// if it has one, ErrNotFound if there is no such network. Its VNI and subnet
// are free again.
func (d *DB) DeleteNetwork(ctx context.Context, name string) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete network: %w", err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM networks WHERE name=?`, name).Scan(&n); err != nil {
		return fmt.Errorf("delete network: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM network_members WHERE network=?`, name).Scan(&n); err != nil {
		return fmt.Errorf("delete network: %w", err)
	}
	if n > 0 {
		return ErrNetworkHasMembers
	}
	// A network with no members has no VMs on other hosts: its records go too.
	if _, err := tx.ExecContext(ctx, `DELETE FROM network_reservations WHERE network=?`, name); err != nil {
		return fmt.Errorf("delete network: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM networks WHERE name=?`, name); err != nil {
		return fmt.Errorf("delete network: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("delete network: %w", err)
	}
	return nil
}

// PutNetworkMember adds m.Host to the network, or updates its LAN address and
// interface (it keeps its joined time). ErrNotFound if there is no such
// network. m.Joined is ignored; the member is returned as stored.
func (d *DB) PutNetworkMember(ctx context.Context, network string, m NetworkMember) (NetworkMember, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return NetworkMember{}, fmt.Errorf("put network member: %w", err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM networks WHERE name=?`, network).Scan(&n); err != nil {
		return NetworkMember{}, fmt.Errorf("put network member: %w", err)
	}
	if n == 0 {
		return NetworkMember{}, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO network_members(network, host, lan_ip, lan_dev, joined) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(network, host) DO UPDATE SET lan_ip=excluded.lan_ip, lan_dev=excluded.lan_dev`,
		network, m.Host, m.LANIP, m.LANDev, d.stamp()); err != nil {
		return NetworkMember{}, fmt.Errorf("put network member: %w", err)
	}
	var joined string
	if err := tx.QueryRowContext(ctx, `SELECT joined FROM network_members WHERE network=? AND host=?`, network, m.Host).Scan(&joined); err != nil {
		return NetworkMember{}, fmt.Errorf("put network member: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return NetworkMember{}, fmt.Errorf("put network member: %w", err)
	}
	m.Joined = parseTime(joined)
	return m, nil
}

// DeleteNetworkMember removes host from the network, or returns ErrNotFound.
func (d *DB) DeleteNetworkMember(ctx context.Context, network, host string) error {
	return affected(d.sql.ExecContext(ctx, `DELETE FROM network_members WHERE network=? AND host=?`, network, host))
}

// ErrAddressTaken: another MAC holds that address on the network.
var ErrAddressTaken = errors.New("address is taken")

// NetworkReservation is the store's copy of one reservation of a network that
// spans hosts: MAC (lower case) and address on a network, for a VM of a host.
type NetworkReservation struct {
	Network string
	MAC     string
	IP      string
	Host    string
	VM      string
	Created time.Time
	// UUID is the VM's libvirt UUID, empty when not known.
	UUID string
	// OrphanedAt is when the record was marked orphaned; zero when it is not.
	OrphanedAt time.Time
}

const resCols = `mac, ip, host, vm, created, uuid, orphaned_at`

// scanReservation reads resCols of one row of network.
func scanReservation(network string, sc interface{ Scan(...any) error }) (NetworkReservation, error) {
	r := NetworkReservation{Network: network}
	var created string
	var orphaned sql.NullString
	if err := sc.Scan(&r.MAC, &r.IP, &r.Host, &r.VM, &created, &r.UUID, &orphaned); err != nil {
		return NetworkReservation{}, err
	}
	r.Created = parseTime(created)
	if orphaned.Valid {
		r.OrphanedAt = parseTime(orphaned.String)
	}
	return r, nil
}

// MarkReservationOrphaned sets orphaned_at to now on the record of mac; a
// record already marked keeps its first time. ErrNotFound if there is no such
// record. The caller knows a positive fact (it could not remove the
// reservation on the home); the store reaches no host.
func (d *DB) MarkReservationOrphaned(ctx context.Context, network, mac string) (NetworkReservation, error) {
	if err := affected(d.sql.ExecContext(ctx,
		`UPDATE network_reservations SET orphaned_at=COALESCE(orphaned_at, ?) WHERE network=? AND mac=?`,
		d.stamp(), network, mac)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return NetworkReservation{}, ErrNotFound
		}
		return NetworkReservation{}, fmt.Errorf("mark reservation orphaned: %w", err)
	}
	r, err := scanReservation(network, d.sql.QueryRowContext(ctx,
		`SELECT `+resCols+` FROM network_reservations WHERE network=? AND mac=?`, network, mac))
	if err != nil {
		return NetworkReservation{}, fmt.Errorf("mark reservation orphaned: %w", err)
	}
	return r, nil
}

// OrphanedReservations lists a network's records that are marked orphaned, in
// address order, or ErrNotFound if there is no such network.
func (d *DB) OrphanedReservations(ctx context.Context, network string) ([]NetworkReservation, error) {
	all, err := d.NetworkReservations(ctx, network)
	if err != nil {
		return nil, err
	}
	out := []NetworkReservation{}
	for _, r := range all {
		if !r.OrphanedAt.IsZero() {
			out = append(out, r)
		}
	}
	return out, nil
}

// PutNetworkReservation records r (idempotent per MAC: the same MAC takes the
// new values and keeps its created time). ErrNotFound if there is no such
// network; ErrAddressTaken if another MAC holds r.IP on it. r.Created is
// ignored; the record is returned as stored.
func (d *DB) PutNetworkReservation(ctx context.Context, r NetworkReservation) (NetworkReservation, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return NetworkReservation{}, fmt.Errorf("put network reservation: %w", err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM networks WHERE name=?`, r.Network).Scan(&n); err != nil {
		return NetworkReservation{}, fmt.Errorf("put network reservation: %w", err)
	}
	if n == 0 {
		return NetworkReservation{}, ErrNotFound
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM network_reservations WHERE network=? AND ip=? AND mac<>?`, r.Network, r.IP, r.MAC).Scan(&n); err != nil {
		return NetworkReservation{}, fmt.Errorf("put network reservation: %w", err)
	}
	if n > 0 {
		return NetworkReservation{}, ErrAddressTaken
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO network_reservations(network, mac, ip, host, vm, created, uuid) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(network, mac) DO UPDATE SET ip=excluded.ip, host=excluded.host, vm=excluded.vm,
		   uuid=CASE WHEN excluded.uuid<>'' THEN excluded.uuid ELSE network_reservations.uuid END,
		   orphaned_at=NULL`,
		r.Network, r.MAC, r.IP, r.Host, r.VM, d.stamp(), r.UUID); err != nil {
		return NetworkReservation{}, fmt.Errorf("put network reservation: %w", err)
	}
	got, err := scanReservation(r.Network, tx.QueryRowContext(ctx,
		`SELECT `+resCols+` FROM network_reservations WHERE network=? AND mac=?`, r.Network, r.MAC))
	if err != nil {
		return NetworkReservation{}, fmt.Errorf("put network reservation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return NetworkReservation{}, fmt.Errorf("put network reservation: %w", err)
	}
	return got, nil
}

// NetworkReservations lists a network's recorded reservations in address
// order, or ErrNotFound if there is no such network.
func (d *DB) NetworkReservations(ctx context.Context, network string) ([]NetworkReservation, error) {
	var n int
	if err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM networks WHERE name=?`, network).Scan(&n); err != nil {
		return nil, fmt.Errorf("network reservations: %w", err)
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT `+resCols+` FROM network_reservations WHERE network=?`, network)
	if err != nil {
		return nil, fmt.Errorf("network reservations: %w", err)
	}
	defer rows.Close()
	out := []NetworkReservation{}
	for rows.Next() {
		r, err := scanReservation(network, rows)
		if err != nil {
			return nil, fmt.Errorf("network reservations: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("network reservations: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return addrLess(out[i].IP, out[j].IP) })
	return out, nil
}

// addrLess orders IPv4 addresses numerically (so .9 is before .10).
func addrLess(a, b string) bool {
	pa, ea := netip.ParseAddr(a)
	pb, eb := netip.ParseAddr(b)
	if ea != nil || eb != nil {
		return a < b
	}
	return pa.Less(pb)
}

// DeleteNetworkReservation removes the record of mac, or returns ErrNotFound.
func (d *DB) DeleteNetworkReservation(ctx context.Context, network, mac string) error {
	return affected(d.sql.ExecContext(ctx, `DELETE FROM network_reservations WHERE network=? AND mac=?`, network, mac))
}
