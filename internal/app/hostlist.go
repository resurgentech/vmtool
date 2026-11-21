package app

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jaredeh/vmtool/pkg/vmtool"
)

// ListTimeout bounds one host's contribution to a combined listing or probe:
// a dead host costs this long, not more, and never delays the others.
const ListTimeout = 5 * time.Second

// HostOpener returns the Manager for a host and the func that releases it
// (hosts.Resolver.Resolve has this shape).
type HostOpener func(host string) (Manager, func(), error)

// HostVMs is one host's row in a combined listing: its VMs, or why it has
// none.
type HostVMs struct {
	Host string
	VMs  []vmtool.VMInfo
	Err  error
}

func timeoutErr(d time.Duration) error { return fmt.Errorf("timed out after %s", d) }

// ListAll lists the VMs of every host in hosts concurrently, each under
// timeout (ListTimeout when zero). The result has one entry per host in the
// order given (callers pass the local host first, then the configured hosts
// sorted); a host that fails to open, fails to list or does not answer in time
// is an entry with Err set, never a failure of the whole call.
//
// Manager.List takes no context, so a host that does not answer is abandoned,
// not cancelled: its goroutine finishes whenever the Manager's own call ends
// (a remote.Manager gives up after its 30 s client timeout; libvirt answers or
// errors) and then releases the Manager. The goroutine count is therefore
// bounded by one per host per call, for at most that long; its result is
// discarded.
func ListAll(ctx context.Context, hosts []string, open HostOpener, timeout time.Duration) []HostVMs {
	if timeout <= 0 {
		timeout = ListTimeout
	}
	out := make([]HostVMs, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = listOne(ctx, h, open, timeout)
		}()
	}
	wg.Wait()
	return out
}

func listOne(ctx context.Context, host string, open HostOpener, timeout time.Duration) HostVMs {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan HostVMs, 1) // buffered: an abandoned goroutine can still send and exit
	go func() {
		m, release, err := open(host)
		if err != nil {
			done <- HostVMs{Host: host, Err: err}
			return
		}
		defer release()
		vms, err := m.List()
		done <- HostVMs{Host: host, VMs: vms, Err: err}
	}()
	select {
	case r := <-done:
		return r
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			return HostVMs{Host: host, Err: timeoutErr(timeout)}
		}
		return HostVMs{Host: host, Err: ctx.Err()}
	}
}

// HostStatus is one host's row of `vmtool hosts` and GET /v1/hosts.
type HostStatus struct {
	Host  string
	Role  string
	Local bool
	// NetworkHome is true for a host whose config.yml entry says vms: false.
	NetworkHome bool
	Address     string
	// Reachable is true when the host answered (the local host always does).
	Reachable  bool
	APIVersion string
	// CertNotAfter is the NotAfter of the leaf certificate the host
	// presented; zero when unknown.
	CertNotAfter time.Time
	// Err says why the host is unreachable, or notes a problem with a host
	// that did answer.
	Err error
}

// CertDaysLeft is the whole days (rounded down, negative once expired) from
// now to the certificate's NotAfter; ok is false when the certificate is
// unknown.
func (s HostStatus) CertDaysLeft(now time.Time) (days int, ok bool) {
	if s.CertNotAfter.IsZero() {
		return 0, false
	}
	return int(math.Floor(s.CertNotAfter.Sub(now).Hours() / 24)), true
}

// Prober is implemented by a Manager that can report its host's status
// (remote.Manager: GET /version over TLS).
type Prober interface {
	Probe(ctx context.Context) HostStatus
}

// ProbeAll probes every host in hosts concurrently under timeout
// (ListTimeout when zero), returning one HostStatus per host in the order
// given. Only hosts whose Manager is a Prober can be probed; any other, and a
// host that does not open, is an unreachable entry with its error.
func ProbeAll(ctx context.Context, hosts []string, open HostOpener, timeout time.Duration) []HostStatus {
	if timeout <= 0 {
		timeout = ListTimeout
	}
	out := make([]HostStatus, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			st := probeOne(cctx, h, open)
			if cctx.Err() == context.DeadlineExceeded && !st.Reachable {
				st.Err = timeoutErr(timeout)
			}
			st.Host = h
			out[i] = st
		}()
	}
	wg.Wait()
	return out
}

func probeOne(ctx context.Context, host string, open HostOpener) HostStatus {
	m, release, err := open(host)
	if err != nil {
		return HostStatus{Err: err}
	}
	defer release()
	p, ok := m.(Prober)
	if !ok {
		return HostStatus{Err: fmt.Errorf("host %q cannot be probed", host)}
	}
	return p.Probe(ctx)
}
