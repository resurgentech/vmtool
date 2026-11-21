package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jaredeh/vmtool/internal/storeapi"
	"github.com/jaredeh/vmtool/internal/storeclient"
	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// The inventory reporter (tranche 12): a hypervisor daemon tells the store
// which VMs it has. One goroutine owns every send. It sends the full list at
// start and after each Kick, never on a timer; a failed send is retried on a
// backoff with a freshly built list. A VM operation only calls Kick, which
// never blocks, so no VM operation depends on the store.
//
// One follow-up exists besides kicks: a VM that is running with no address
// when a list is built is watched (by UUID) for up to IPWait. While any VM is
// watched, the same goroutine re-reads the VM list every 5 s and sends one full
// report when a watched VM has an address. With nothing watched there is no
// timer. A VM is watched once per run of its domain: it is forgotten when a
// list shows it not running (or gone), and a build the watch itself triggers
// adds no VM.

// ReportStore is the part of *storeclient.Client the reporter uses.
type ReportStore interface {
	PutHostInventory(ctx context.Context, host string, in storeclient.HostReport) (*storeclient.ReportResult, error)
}

// InventoryDisk is one disk of a VM as the report records it.
type InventoryDisk struct {
	Target        string
	Pool          string
	Kind          string // file, block or thin
	CapacityBytes int64
}

// InventoryReader is one consistent look at the host's VMs. List must carry
// each VM's UUID, firmware, origin and comment.
type InventoryReader interface {
	List() ([]vmtool.VMInfo, error)
	// VMDisks is the VM's disks; an error wrapping vmtool.ErrNotFound means
	// the VM went away since List.
	VMDisks(name string) ([]InventoryDisk, error)
}

// InventorySource opens an InventoryReader (one libvirt connection, say) for
// one build of the list; done releases it.
type InventorySource interface {
	Open() (r InventoryReader, done func(), err error)
}

// Backoff after a failed send: 30 s doubling to 5 min.
const (
	reportBackoffMin = 30 * time.Second
	reportBackoffMax = 5 * time.Minute
	reportSendLimit  = 10 * time.Second
	watchPoll        = 5 * time.Second
)

// InventoryReporterConfig is what NewInventoryReporter needs. Now, After, Log
// and Timeout have defaults; tests inject a clock.
type InventoryReporterConfig struct {
	Host    string // the daemon's own name: the path of the report
	Version string // the daemon's build version
	Store   ReportStore
	Source  InventorySource
	Now     func() time.Time
	After   func(time.Duration) <-chan time.Time
	Log     *slog.Logger
	// IPWait is how long a running VM with no address is watched for one
	// (the daemon's ip_wait_seconds). 0 turns the follow-up off.
	IPWait time.Duration
	// Timeout bounds one send; 0 means 10 s.
	Timeout time.Duration
}

// InventoryReporter is the reporter. Build it with NewInventoryReporter.
type InventoryReporter struct {
	c    InventoryReporterConfig
	kick chan struct{}

	once sync.Once
	done chan struct{}

	// Touched only by the reporter goroutine.
	runs       map[string]*vmRun // by UUID: VMs seen running, so each run is handled once
	watching   int               // how many of runs are still watched
	unreadable map[string]bool   // VM names whose disks could not be read at the last build
}

// vmRun is one run of a VM's domain as the reporter saw it. A VM with a
// deadline is being watched for an address.
type vmRun struct {
	watched  bool
	deadline time.Time
}

// NewInventoryReporter returns a reporter that has not started.
func NewInventoryReporter(c InventoryReporterConfig) *InventoryReporter {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.After == nil {
		c.After = time.After
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Timeout <= 0 {
		c.Timeout = reportSendLimit
	}
	return &InventoryReporter{c: c, kick: make(chan struct{}, 1), done: make(chan struct{}),
		runs: map[string]*vmRun{}, unreadable: map[string]bool{}}
}

// IPWait is how long a running VM without an address is watched for one; 0
// means the follow-up is off.
func (r *InventoryReporter) IPWait() time.Duration { return r.c.IPWait }

// Kick marks the list as changed. It never blocks and coalesces: any number of
// kicks while a send runs cause one more send.
func (r *InventoryReporter) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Start runs the one reporter goroutine (a second call starts nothing) and
// returns a channel closed when it has stopped, which happens when ctx ends.
// The first send happens at once, in the background.
func (r *InventoryReporter) Start(ctx context.Context) <-chan struct{} {
	r.once.Do(func() { go r.run(ctx) })
	return r.done
}

type reportState int

const (
	stateNone reportState = iota
	stateOK
	stateDenied
	stateFailed
	stateListing
)

func (r *InventoryReporter) run(ctx context.Context) {
	defer close(r.done)
	var (
		state     reportState
		backoff   time.Duration
		pollC     <-chan time.Time // the one outstanding poll timer, if any
		fromWatch bool             // this attempt was triggered by the watch
	)
	for {
		st, err := r.attempt(ctx, fromWatch)
		fromWatch = false
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = 0
			if state != st {
				if state == stateNone {
					r.c.Log.Info("inventory reported to the store", "host", r.c.Host)
				} else {
					r.c.Log.Info("inventory reporting resumed", "host", r.c.Host)
				}
				state = st
			}
		wait:
			for {
				if pollC == nil && r.watching > 0 {
					pollC = r.c.After(watchPoll)
				}
				select {
				case <-r.kick:
					break wait
				case <-pollC:
					pollC = nil
					if r.poll() {
						fromWatch = true
						break wait
					}
				case <-ctx.Done():
					return
				}
			}
			continue
		}
		if state != st {
			switch st {
			case stateDenied:
				r.c.Log.Warn("the store refused the inventory report (host token not accepted); retrying every 5 minutes", "err", err)
			case stateListing:
				r.c.Log.Warn("inventory not reported: the VM list could not be read; will retry", "err", err)
			default:
				r.c.Log.Warn("inventory not reported: the store did not take it; will retry", "err", err)
			}
			state = st
		}
		switch {
		case st == stateDenied:
			backoff = reportBackoffMax
		case backoff == 0:
			backoff = reportBackoffMin
		default:
			backoff = min(backoff*2, reportBackoffMax)
		}
		select {
		case <-r.c.After(backoff):
		case <-ctx.Done():
			return
		}
	}
}

// observe updates the watch from one reading of the host's VMs. A VM seen not
// running (or no longer listed) ends its run: it is forgotten, so its next run
// is watched afresh. A watched VM with an address, or whose window is over, is
// no longer watched; the first reports true. With add, a running VM not yet
// seen in this run is recorded, and watched when it has no address.
func (r *InventoryReporter) observe(vms []vmtool.VMInfo, add bool) (gotAddress bool) {
	if r.c.IPWait <= 0 {
		return false
	}
	now := r.c.Now()
	seen := make(map[string]bool, len(vms))
	for _, v := range vms {
		if v.State != vmtool.StateRunning || v.UUID == "" {
			continue
		}
		seen[v.UUID] = true
		run := r.runs[v.UUID]
		if run == nil {
			if !add {
				continue
			}
			run = &vmRun{}
			r.runs[v.UUID] = run
			if v.IP == "" {
				run.watched, run.deadline = true, now.Add(r.c.IPWait)
				r.watching++
			}
			continue
		}
		if !run.watched {
			continue
		}
		switch {
		case v.IP != "":
			run.watched = false
			r.watching--
			gotAddress = true
		case !now.Before(run.deadline):
			run.watched = false // given up; the run stays recorded
			r.watching--
		}
	}
	for u, run := range r.runs {
		if !seen[u] {
			if run.watched {
				r.watching--
			}
			delete(r.runs, u)
		}
	}
	return gotAddress
}

// poll re-reads the VM list for the watch, no disks and no send. It reports
// whether a watched VM now has an address, so the caller sends one report. A
// read error changes nothing but the window, which still runs out on the clock.
func (r *InventoryReporter) poll() bool {
	var vms []vmtool.VMInfo
	rd, done, err := r.c.Source.Open()
	if err == nil {
		defer done()
		vms, err = rd.List()
	}
	if err != nil {
		now := r.c.Now()
		for _, run := range r.runs {
			if run.watched && !now.Before(run.deadline) {
				run.watched = false
				r.watching--
			}
		}
		return false
	}
	return r.observe(vms, false)
}

// attempt builds a fresh full list and sends it. Kicks that arrived before
// the build are covered by it, so they are dropped here; one that arrives
// during the send stays pending.
func (r *InventoryReporter) attempt(ctx context.Context, fromWatch bool) (reportState, error) {
	select {
	case <-r.kick:
	default:
	}
	rep, err := r.build(!fromWatch)
	if err != nil {
		return stateListing, err
	}
	sctx, cancel := context.WithTimeout(ctx, r.c.Timeout)
	defer cancel()
	if _, err := r.c.Store.PutHostInventory(sctx, r.c.Host, rep); err != nil {
		if errors.Is(err, storeclient.ErrUnauthorized) || errors.Is(err, storeclient.ErrForbidden) {
			return stateDenied, err
		}
		return stateFailed, err
	}
	return stateOK, nil
}

// build lists the host's VMs. A listing error is an error and sends nothing: a
// VM missing from a report is delisted. A VM whose disks cannot be read for a
// reason other than it having just vanished is still reported, marked
// disks_unreadable with no disks, so the rest of the report goes out and the
// VM is not delisted. It also updates the address watch from the list it read
// (add: this build may start watching VMs; the watch's own build may not).
func (r *InventoryReporter) build(add bool) (storeclient.HostReport, error) {
	rd, done, err := r.c.Source.Open()
	if err != nil {
		return storeclient.HostReport{}, fmt.Errorf("listing VMs: %w", err)
	}
	defer done()
	vms, err := rd.List()
	if err != nil {
		return storeclient.HostReport{}, fmt.Errorf("listing VMs: %w", err)
	}
	out := make([]storeclient.ReportVM, 0, len(vms))
	unreadable := map[string]bool{}
	for _, v := range vms {
		if v.UUID == "" {
			return storeclient.HostReport{}, fmt.Errorf("listing VMs: %q has no UUID", v.Name)
		}
		disks, err := rd.VMDisks(v.Name)
		if vmtool.IsNotFound(err) {
			continue // removed since the list: not part of this report
		}
		rv := reportVM(v, disks)
		if err != nil {
			yes := true
			rv.DisksUnreadable = &yes
			if !r.unreadable[v.Name] {
				r.c.Log.Warn("a VM's disks could not be read; reporting it without them", "vm", v.Name, "err", err)
			}
			unreadable[v.Name] = true
		}
		out = append(out, rv)
	}
	r.unreadable = unreadable
	r.observe(vms, add)
	return storeclient.HostReport{GeneratedAt: r.c.Now(), DaemonVersion: r.c.Version, Vms: out}, nil
}

// reportVM is one VM of the report: what its host knows. It carries no
// origin and no comment: those are the VM's notes, which only the
// workstation writes to the store.
func reportVM(v vmtool.VMInfo, disks []InventoryDisk) storeclient.ReportVM {
	rv := storeclient.ReportVM{
		Name: v.Name, Uuid: v.UUID, State: string(v.State), Vcpus: int(v.VCPUs), Memory: int(v.Memory),
		Ip: v.IP, Autostart: v.Autostart, Firmware: v.Firmware,
		Disks: make([]storeapi.Disk, 0, len(disks)),
	}
	for _, d := range disks {
		rv.Disks = append(rv.Disks, storeapi.Disk{Target: d.Target, Pool: d.Pool, Kind: storeapi.DiskKind(d.Kind), CapacityBytes: d.CapacityBytes})
	}
	if len(v.Networks) > 0 {
		nets := make([]storeapi.VMNetwork, len(v.Networks))
		for i, n := range v.Networks {
			nets[i] = storeapi.VMNetwork{Network: n.Network, Mac: n.MAC, Ip: n.IP}
		}
		rv.Networks = &nets
	}
	return rv
}
