// Package fake is an in-memory Dataplane for tests.
package fake

import (
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
	Routes    map[netip.Addr]string
	Log       []string
	SetupDone bool
	TornDown  bool
}

func New() *Dataplane {
	return &Dataplane{Routes: map[netip.Addr]string{}}
}

func (f *Dataplane) Setup(down dataplane.Interface, ups []dataplane.Interface, mtu int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Down, f.Ups, f.MTU, f.SetupDone = down, ups, mtu, true
	f.Log = append(f.Log, fmt.Sprintf("setup %s mtu=%d ups=%d", down.Name, mtu, len(ups)))
	return nil
}

func (f *Dataplane) Route(peer netip.Addr, upstream string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.Ups {
		if u.Name == upstream {
			f.Routes[peer] = upstream
			f.Log = append(f.Log, fmt.Sprintf("route %s via %s", peer, upstream))
			return nil
		}
	}
	return fmt.Errorf("unknown upstream %q", upstream)
}

func (f *Dataplane) Unroute(peer netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Routes, peer)
	f.Log = append(f.Log, fmt.Sprintf("unroute %s", peer))
	return nil
}

func (f *Dataplane) Teardown() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Routes = map[netip.Addr]string{}
	f.TornDown = true
	f.Log = append(f.Log, "teardown")
	return nil
}

// RouteOf returns the upstream a peer is currently routed through.
func (f *Dataplane) RouteOf(peer netip.Addr) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.Routes[peer]
	return u, ok
}
