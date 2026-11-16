// Package fake is an in-memory Dataplane for tests.
package fake

import (
	"context"
	"fmt"
	"net/netip"
	"sync"

	"github.com/anesthetised/lotsman/internal/dataplane"
)

type Dataplane struct {
	mu        sync.Mutex
	Down      dataplane.Interface
	Ups       []dataplane.Interface
	MTU       int
	Metrics   netip.AddrPort
	Routes    map[netip.Addr]string
	Pins      map[netip.Addr]map[string]bool // peer → upstreams its old flows stay on
	Alive     map[string]bool
	Allowed   map[netip.Addr]bool // peers the firewall admits; Route admits no one while Flushed
	Flushed   bool                // the firewall is gone until Verify reinstalls it
	Log       []string
	SetupDone bool
	TornDown  bool

	watch    chan error // the channel of a running Watch, nil otherwise
	verifies int        // Verify calls
}

func New() *Dataplane {
	return &Dataplane{Routes: map[netip.Addr]string{}, Pins: map[netip.Addr]map[string]bool{}, Alive: map[string]bool{},
		Allowed: map[netip.Addr]bool{}}
}

func (f *Dataplane) Setup(down dataplane.Interface, ups []dataplane.Interface, mtu int, metrics netip.AddrPort) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Down, f.Ups, f.MTU, f.Metrics, f.SetupDone = down, ups, mtu, metrics, true
	f.Allowed, f.Flushed = map[netip.Addr]bool{}, false
	f.Log = append(f.Log, fmt.Sprintf("setup %s mtu=%d ups=%d", down.Name, mtu, len(ups)))
	return nil
}

func (f *Dataplane) SetUpstreams(ups []dataplane.Interface, mtu int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Ups, f.MTU = ups, mtu
	f.Log = append(f.Log, fmt.Sprintf("set-upstreams mtu=%d ups=%d", mtu, len(ups)))
	return nil
}

func (f *Dataplane) Route(peer netip.Addr, upstream string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.Ups {
		if u.Name == upstream {
			if prev, ok := f.Routes[peer]; ok && prev != upstream && f.Alive[prev] {
				if f.Pins[peer] == nil {
					f.Pins[peer] = map[string]bool{}
				}
				f.Pins[peer][prev] = true
			}
			delete(f.Pins[peer], upstream)
			f.Routes[peer] = upstream
			if !f.Flushed {
				f.Allowed[peer] = true
			}
			f.Log = append(f.Log, fmt.Sprintf("route %s via %s", peer, upstream))
			return nil
		}
	}
	return fmt.Errorf("unknown upstream %q", upstream)
}

func (f *Dataplane) SetAlive(upstream string, alive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Alive[upstream] = alive
	if !alive {
		for _, pins := range f.Pins {
			delete(pins, upstream)
		}
	}
	f.Log = append(f.Log, fmt.Sprintf("alive %s=%v", upstream, alive))
	return nil
}

// PinnedTo reports whether a peer's old flows are kept on the given upstream.
func (f *Dataplane) PinnedTo(peer netip.Addr, upstream string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Pins[peer][upstream]
}

func (f *Dataplane) Unroute(peer netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Routes, peer)
	delete(f.Pins, peer)
	delete(f.Allowed, peer)
	f.Log = append(f.Log, fmt.Sprintf("unroute %s", peer))
	return nil
}

// FlushFirewall removes the firewall as "nft flush ruleset" would, and
// reports it to a running Watch as the kernel would.
func (f *Dataplane) FlushFirewall() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Flushed = true
	f.Allowed = map[netip.Addr]bool{}
	f.notify(nil)
}

// Verify reinstalls a flushed firewall. Like the real one, the reinstall is
// itself a change a running Watch reports.
func (f *Dataplane) Verify() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verifies++
	if !f.Flushed {
		return false, nil
	}
	f.Flushed = false
	f.Log = append(f.Log, "verify reinstalled")
	f.notify(nil)
	return true, nil
}

// Verifies counts Verify calls.
func (f *Dataplane) Verifies() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.verifies
}

// Watch returns a channel that Notify, FlushFirewall and a repairing Verify
// send to, closed once ctx is done.
func (f *Dataplane) Watch(ctx context.Context) <-chan error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan error, 1)
	f.watch = ch
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		defer f.mu.Unlock()
		close(ch)
		if f.watch == ch {
			f.watch = nil
		}
	}()
	return ch
}

// Notify sends err to a running Watch: nil reports a change, an error a
// failed watch. A value is dropped while the previous one is unread.
func (f *Dataplane) Notify(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notify(err)
}

func (f *Dataplane) notify(err error) {
	if f.watch == nil {
		return
	}
	select {
	case f.watch <- err:
	default:
	}
}

// IsAllowed reports whether the firewall admits the peer.
func (f *Dataplane) IsAllowed(peer netip.Addr) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Allowed[peer]
}

func (f *Dataplane) Teardown() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Routes = map[netip.Addr]string{}
	f.TornDown = true
	f.Log = append(f.Log, "teardown")
	return nil
}

// Ready reports whether Setup has run.
func (f *Dataplane) Ready() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.SetupDone
}

// RouteOf returns the upstream a peer is currently routed through.
func (f *Dataplane) RouteOf(peer netip.Addr) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.Routes[peer]
	return u, ok
}
