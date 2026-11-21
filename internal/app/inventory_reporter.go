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
	// Timeout bounds one send; 0 means 10 s.
	Timeout time.Duration
}

// InventoryReporter is the reporter. Build it with NewInventoryReporter.
type InventoryReporter struct {
	c    InventoryReporterConfig
	kick chan struct{}

	once sync.Once
	done chan struct{}
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
	return &InventoryReporter{c: c, kick: make(chan struct{}, 1), done: make(chan struct{})}
}

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
		state   reportState
		backoff time.Duration
	)
	for {
		st, err := r.attempt(ctx)
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
			select {
			case <-r.kick:
				continue
			case <-ctx.Done():
				return
			}
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

// attempt builds a fresh full list and sends it. Kicks that arrived before
// the build are covered by it, so they are dropped here; one that arrives
// during the send stays pending.
func (r *InventoryReporter) attempt(ctx context.Context) (reportState, error) {
	select {
	case <-r.kick:
	default:
	}
	rep, err := r.build()
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

// build lists the host's VMs. A listing error, or any VM whose disks cannot
// be read for a reason other than it having just vanished, is an error and
// sends nothing: a VM missing from a report is delisted.
func (r *InventoryReporter) build() (storeclient.HostReport, error) {
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
	for _, v := range vms {
		if v.UUID == "" {
			return storeclient.HostReport{}, fmt.Errorf("listing VMs: %q has no UUID", v.Name)
		}
		disks, err := rd.VMDisks(v.Name)
		if vmtool.IsNotFound(err) {
			continue // removed since the list: not part of this report
		}
		if err != nil {
			return storeclient.HostReport{}, fmt.Errorf("reading the disks of %q: %w", v.Name, err)
		}
		out = append(out, reportVM(v, disks))
	}
	return storeclient.HostReport{GeneratedAt: r.c.Now(), DaemonVersion: r.c.Version, Vms: out}, nil
}

func reportVM(v vmtool.VMInfo, disks []InventoryDisk) storeclient.ReportVM {
	comment := v.Comment // always sent: "" clears the store's copy
	rv := storeclient.ReportVM{
		Name: v.Name, Uuid: v.UUID, State: string(v.State), Vcpus: int(v.VCPUs), Memory: int(v.Memory),
		Ip: v.IP, Autostart: v.Autostart, Firmware: v.Firmware, Comment: &comment,
		Disks: make([]storeapi.Disk, 0, len(disks)),
	}
	for _, d := range disks {
		rv.Disks = append(rv.Disks, storeapi.Disk{Target: d.Target, Pool: d.Pool, Kind: storeapi.DiskKind(d.Kind), CapacityBytes: d.CapacityBytes})
	}
	if o := v.Origin; o != nil {
		so := &storeapi.VMOrigin{}
		set := func(dst **string, s string) {
			if s != "" {
				*dst = &s
			}
		}
		set(&so.Image, o.Image)
		set(&so.ImageSha256, o.ImageSHA256)
		set(&so.Template, o.Template)
		set(&so.Clone, o.CloneKind)
		set(&so.Pool, o.Pool)
		if !o.Created.IsZero() {
			set(&so.Created, o.Created.UTC().Format(time.RFC3339))
		}
		rv.Origin = so
	}
	return rv
}
