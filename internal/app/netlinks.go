package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// The daemon keeps its links up (tranche 17, V5). A VXLAN device does not
// survive a reboot, and a libvirt network that is stopped and started again
// gets a new bridge, so the daemon links, in the background:
//
//   - at start, every network of this host whose bridge is vmtool's (found by
//     the bridge name; its digits are the VNI);
//   - after libvirt's "network started" event, that network.
//
// What it links to comes from the store, with the host's own token
// (GET /v1/hosts/{host}/networks): the host's own row (its LAN address and
// interface) and its peers. It links a network only when the registry's network
// of that VNI has the same name and lists this host: VNIs of deleted networks
// are reused, so a stale bridge must not join a new network. Otherwise it
// leaves the bridge unlinked and says so once. With the store unreachable it
// makes no link from guesses: it says so once and retries on the reporter's
// backoff (30 s doubling to 5 min). It never blocks serving: Start returns at
// once. A host with no vmtool network never calls the store.
//
// vmtool never restarts a network or libvirtd. After a manual net-destroy and
// net-start the VMs' cards stay detached from the new bridge; the daemon
// re-links the network and logs how to repair the cards (restart libvirtd).

// LinkStore is the part of *storeclient.Client the keeper uses.
type LinkStore interface {
	HostNetworks(ctx context.Context, host string) (*storeclient.HostNetworks, error)
}

// NetLinkKeeperConfig is what NewNetLinkKeeper needs. After and Log have
// defaults; tests inject a clock.
type NetLinkKeeperConfig struct {
	Host  string // the daemon's own name
	Store LinkStore
	// Open opens this host's libvirt for one pass; done releases it.
	Open func() (m Manager, done func(), err error)
	// Watch registers fn to be called with the name of each libvirt network
	// that starts, and returns the func that stops it. nil: no events.
	Watch func(fn func(name string)) (stop func(), err error)
	After func(time.Duration) <-chan time.Time
	Log   *slog.Logger
	// Timeout bounds one store read; 0 means 10 s.
	Timeout time.Duration
}

// NetLinkKeeper is the keeper. Build it with NewNetLinkKeeper.
type NetLinkKeeper struct {
	c NetLinkKeeperConfig

	mu      sync.Mutex
	want    map[string]bool // networks to link at the next pass
	all     bool            // every network
	events  map[string]bool // of want, the ones a started event asked for
	wake    chan struct{}
	started bool
	done    chan struct{}

	// Touched only by the keeper goroutine.
	said map[string]bool // what has been logged, so each thing is said once

	// passDone, for tests, is called at the end of every pass.
	passDone func(retry bool)
}

// NewNetLinkKeeper returns a keeper that has not started.
func NewNetLinkKeeper(c NetLinkKeeperConfig) *NetLinkKeeper {
	if c.After == nil {
		c.After = time.After
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Timeout <= 0 {
		c.Timeout = reportSendLimit
	}
	return &NetLinkKeeper{c: c, want: map[string]bool{}, events: map[string]bool{}, wake: make(chan struct{}, 1),
		done: make(chan struct{}), said: map[string]bool{}}
}

// Relink asks for a link of the network name ("" for every network). It never
// blocks, and asks coalesce. It is what the "network started" event calls.
func (k *NetLinkKeeper) Relink(name string) { k.ask(name, false) }

func (k *NetLinkKeeper) ask(name string, event bool) {
	k.mu.Lock()
	if name == "" {
		k.all = true
	} else {
		k.want[name] = true
		if event {
			k.events[name] = true
		}
	}
	k.mu.Unlock()
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// Start runs the one goroutine (a second call starts nothing) and returns a
// channel closed when it has stopped, which happens when ctx ends. The first
// pass happens at once, in the background.
func (k *NetLinkKeeper) Start(ctx context.Context) <-chan struct{} {
	k.mu.Lock()
	first := !k.started
	k.started = true
	k.all = true
	k.mu.Unlock()
	if first {
		go k.run(ctx)
	}
	return k.done
}

func (k *NetLinkKeeper) run(ctx context.Context) {
	defer close(k.done)
	if k.c.Watch != nil {
		stop, err := k.c.Watch(func(name string) { k.ask(name, true) })
		if err != nil {
			k.c.Log.Warn("network links: libvirt's network events are not available, so a network that libvirt starts again is not re-linked until the daemon restarts: " + err.Error())
		} else {
			defer stop()
		}
	}
	backoff := reportBackoffMin
	for {
		retry := k.pass(ctx)
		if k.passDone != nil {
			k.passDone(retry)
		}
		if retry {
			select {
			case <-ctx.Done():
				return
			case <-k.wake: // an event or a Relink: try at once
			case <-k.c.After(backoff):
			}
			if backoff *= 2; backoff > reportBackoffMax {
				backoff = reportBackoffMax
			}
			// A wake that is no retry timer does not reset the backoff; only
			// a pass that needs no retry does.
			continue
		}
		backoff = reportBackoffMin
		select {
		case <-ctx.Done():
			return
		case <-k.wake:
		}
	}
}

// say logs line once per key.
func (k *NetLinkKeeper) say(key string, level slog.Level, line string) {
	if k.said[key] {
		return
	}
	k.said[key] = true
	k.c.Log.Log(context.Background(), level, line)
}

// resolved clears the "said" marks that begin with prefix: the thing is right
// now, so if it breaks again it is worth a line again.
func (k *NetLinkKeeper) resolved(prefix string) {
	for key := range k.said {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(k.said, key)
		}
	}
}

// pass links the wanted networks and reports whether a retry is needed (the
// store did not answer, or a link failed).
func (k *NetLinkKeeper) pass(ctx context.Context) (retry bool) {
	k.mu.Lock()
	all, want, events := k.all, k.want, k.events
	k.all, k.want, k.events = false, map[string]bool{}, map[string]bool{}
	k.mu.Unlock()

	m, done, err := k.c.Open()
	if err != nil {
		k.say("open:"+err.Error(), slog.LevelWarn, "network links: cannot open libvirt, so no network is linked yet; will retry: "+err.Error())
		k.requeue(all, want)
		return true
	}
	defer done()
	k.resolved("open:")
	nm, canLink := AsNetMember(m)

	names, err := m.ListNetworks()
	if err != nil {
		k.say("list:"+err.Error(), slog.LevelWarn, "network links: cannot list libvirt networks; will retry: "+err.Error())
		k.requeue(all, want)
		return true
	}
	type local struct {
		name string
		info *vmtool.NetworkInfo
	}
	var nets []local
	for _, name := range names {
		if !all && !want[name] {
			continue
		}
		info, err := m.Network(name)
		if err != nil || info == nil || !info.Vmtool {
			continue
		}
		nets = append(nets, local{name, info})
		if events[name] {
			k.c.Log.Info(fmt.Sprintf("network links: libvirt started network %q again, so its bridge %s is new: linking it again. VMs that were attached before keep a card on the old bridge and cannot reach the network until libvirt is restarted (sudo systemctl restart libvirtd); vmtool never restarts it", name, info.Bridge))
		}
	}
	if len(nets) == 0 {
		return false
	}
	if !canLink {
		k.say("nolink", slog.LevelWarn, "network links: this host's libvirt connection cannot make links (a daemon without network support)")
		return false
	}

	sctx, cancel := context.WithTimeout(ctx, k.c.Timeout)
	hn, err := k.c.Store.HostNetworks(sctx, k.c.Host)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		k.say("store", slog.LevelWarn, fmt.Sprintf("network links: the store did not answer, so no link is made yet for %s (no link is made from guesses); will retry: %v", netNames(nets2names(nets, func(l local) string { return l.name })), err))
		k.requeue(all, want)
		return true
	}
	k.resolved("store")

	retry = false
	byVNI := map[uint32]storeclient.HostNetwork{}
	for _, n := range hn.Networks {
		byVNI[uint32(n.Vni)] = n
	}
	for _, l := range nets {
		key := "skip:" + l.name
		rn, ok := byVNI[l.info.VNI]
		switch {
		case !ok:
			k.say(key, slog.LevelInfo, fmt.Sprintf("network links: network %q (bridge %s) is not linked: the registry has no network with VNI %d for this host", l.name, l.info.Bridge, l.info.VNI))
			continue
		case rn.Name != l.name:
			k.say(key, slog.LevelInfo, fmt.Sprintf("network links: network %q (bridge %s) is not linked: the registry's network of VNI %d is called %q (a stale bridge of a deleted network?)", l.name, l.info.Bridge, l.info.VNI, rn.Name))
			continue
		case rn.Member == nil && rn.Home == k.c.Host:
			continue // a one-host network: the home with no members has nothing to link
		case rn.Member == nil:
			k.say(key, slog.LevelInfo, fmt.Sprintf("network links: network %q (bridge %s) is not linked: the registry does not list this host as a member of it", l.name, l.info.Bridge))
			continue
		}
		var peers []string
		for _, p := range rn.Peers {
			peers = append(peers, p.LanIp)
		}
		if err := nm.SetNetworkLinks(l.name, rn.Member.LanIp, rn.Member.LanDev, peers); err != nil {
			k.say("link:"+l.name+":"+err.Error(), slog.LevelWarn, fmt.Sprintf("network links: linking network %q failed; will retry: %v", l.name, err))
			k.requeueName(l.name)
			retry = true
			continue
		}
		k.resolved("link:" + l.name + ":")
		delete(k.said, key)
	}
	return retry
}

func nets2names[T any](in []T, f func(T) string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func netNames(names []string) string {
	s := ""
	for i, n := range names {
		if i > 0 {
			s += ", "
		}
		s += n
	}
	return s
}

func (k *NetLinkKeeper) requeue(all bool, want map[string]bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.all = k.all || all
	for n := range want {
		k.want[n] = true
	}
}

func (k *NetLinkKeeper) requeueName(name string) {
	k.mu.Lock()
	k.want[name] = true
	k.mu.Unlock()
}
