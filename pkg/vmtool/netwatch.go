package vmtool

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"libvirt.org/go/libvirt"
)

// The network-started watch that survives a libvirtd restart (G4). The watch
// holds one libvirt connection with keepalive and a close callback; when the
// connection closes it reconnects with backoff, registers the callback again
// and reports one recovery (the caller asks for a re-link pass of every
// network). It sits behind a small connector interface so tests use a fake.

const (
	watchBackoffMin = time.Second
	watchBackoffMax = 30 * time.Second
	// libvirt keepalive: a probe every watchKeepAliveInterval seconds, the
	// connection closed after watchKeepAliveCount unanswered probes.
	watchKeepAliveInterval = 5
	watchKeepAliveCount    = 3
)

// watchConn is one libvirt connection as the watch uses it.
type watchConn interface {
	// SetKeepAlive turns libvirt's keepalive on, so a dead libvirtd is noticed.
	SetKeepAlive(interval int, count uint) error
	// OnClose registers cb to run when the connection closes. cb runs on
	// libvirt's event goroutine and must not block.
	OnClose(cb func()) error
	// OnNetworkStarted registers cb for each network that starts; the returned
	// func deregisters it.
	OnNetworkStarted(cb func(name string)) (deregister func(), err error)
	// Close closes the connection (and drops the close callback).
	Close()
}

// watchConnector opens a watchConn.
type watchConnector func() (watchConn, error)

// watchClock is the time the backoff uses; tests give a fake.
type watchClock interface {
	// After returns a channel that receives once d has passed, and a func that
	// cancels the wait.
	After(d time.Duration) (<-chan time.Time, func())
}

type realWatchClock struct{}

func (realWatchClock) After(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// netWatch is a running watch.
type netWatch struct {
	connect watchConnector
	clock   watchClock
	fn      func(string)
	log     *slog.Logger

	done chan struct{} // closed by stop
	exit chan struct{} // closed when the goroutine has returned
	once sync.Once
}

// startNetWatch opens the first connection (an error is returned to the
// caller) and starts the goroutine that watches it. fn is called with each
// started network's name, and with "" once after each recovery (every
// network). stop ends everything; it does not wait for a connect in progress,
// which closes its connection as soon as it returns.
func startNetWatch(connect watchConnector, clock watchClock, fn func(string), log *slog.Logger) (*netWatch, error) {
	if log == nil {
		log = slog.Default()
	}
	w := &netWatch{connect: connect, clock: clock, fn: fn, log: log,
		done: make(chan struct{}), exit: make(chan struct{})}
	live, err := w.open()
	if err != nil {
		return nil, err
	}
	go w.run(live)
	return w, nil
}

// liveConn is a connection with what closes it down again.
type liveConn struct {
	conn       watchConn
	deregister func()
	closed     chan struct{} // signalled by the close callback
}

func (l *liveConn) shut() {
	if l.deregister != nil {
		l.deregister()
	}
	l.conn.Close()
}

// open connects, sets keepalive and registers both callbacks.
func (w *netWatch) open() (*liveConn, error) {
	c, err := w.connect()
	if err != nil {
		return nil, fmt.Errorf("connecting to libvirt: %w", err)
	}
	l := &liveConn{conn: c, closed: make(chan struct{}, 1)}
	if err := c.SetKeepAlive(watchKeepAliveInterval, watchKeepAliveCount); err != nil {
		c.Close()
		return nil, fmt.Errorf("setting libvirt keepalive: %w", err)
	}
	if err := c.OnClose(func() {
		select {
		case l.closed <- struct{}{}:
		default:
		}
	}); err != nil {
		c.Close()
		return nil, fmt.Errorf("watching the libvirt connection: %w", err)
	}
	dereg, err := c.OnNetworkStarted(func(name string) { w.fn(name) })
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("watching libvirt networks: %w", err)
	}
	l.deregister = dereg
	return l, nil
}

func (w *netWatch) run(live *liveConn) {
	defer close(w.exit)
	for {
		select {
		case <-w.done:
			live.shut()
			return
		case <-live.closed:
		}
		w.log.Warn("libvirt connection lost; network-started watch is reconnecting")
		live.shut()
		var ok bool
		if live, ok = w.reconnect(); !ok {
			return
		}
		w.log.Info("libvirt connection recovered; network-started watch is registered again")
		w.fn("")
	}
}

// reconnect retries with backoff until it has a connection, or stop ends it.
// It waits only between attempts: nothing runs while connected.
func (w *netWatch) reconnect() (*liveConn, bool) {
	delay := watchBackoffMin
	for {
		select {
		case <-w.done:
			return nil, false
		default:
		}
		live, err := w.open()
		if err == nil {
			select {
			case <-w.done:
				live.shut()
				return nil, false
			default:
			}
			return live, true
		}
		ch, cancel := w.clock.After(delay)
		select {
		case <-w.done:
			cancel()
			return nil, false
		case <-ch:
		}
		if delay *= 2; delay > watchBackoffMax {
			delay = watchBackoffMax
		}
	}
}

func (w *netWatch) stop() { w.once.Do(func() { close(w.done) }) }

// libvirtWatchConn is the real watchConn over the binding.
type libvirtWatchConn struct{ c *libvirt.Connect }

func (l libvirtWatchConn) SetKeepAlive(interval int, count uint) error {
	return l.c.SetKeepAlive(interval, count)
}

func (l libvirtWatchConn) OnClose(cb func()) error {
	return l.c.RegisterCloseCallback(func(*libvirt.Connect, libvirt.ConnectCloseReason) { cb() })
}

func (l libvirtWatchConn) OnNetworkStarted(cb func(string)) (func(), error) {
	id, err := l.c.NetworkEventLifecycleRegister(nil, func(_ *libvirt.Connect, n *libvirt.Network, ev *libvirt.NetworkEventLifecycle) {
		if ev.Event != libvirt.NETWORK_EVENT_STARTED {
			return
		}
		if name, err := n.GetName(); err == nil {
			cb(name)
		}
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = l.c.NetworkEventDeregister(id) }, nil
}

func (l libvirtWatchConn) Close() {
	_ = l.c.UnregisterCloseCallback()
	_, _ = l.c.Close()
}

// realWatchConnector opens a connection through connect (refused under go test).
func realWatchConnector() (watchConn, error) {
	c, err := connect("qemu:///system")
	if err != nil {
		return nil, err
	}
	return libvirtWatchConn{c}, nil
}
