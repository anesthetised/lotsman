package daemon

import (
	"context"
	"time"

	"github.com/anesthetised/lotsman/internal/traffic"
)

// TrafficInterval is how often the daemon samples its byte counters. Every
// reader shares the samples, so it does not grow with the viewers.
const TrafficInterval = time.Second

// CountersFunc reads an interface's byte counters as the kernel keeps them,
// as traffic.InterfaceCounters does.
type CountersFunc func(iface string) (rx, tx uint64, err error)

// Counters reads the byte counters of every peer, from the downstream
// device, and of every upstream, from its interface. An upstream's
// interface counts what crosses it whatever the kind, so AWG and proxy
// upstreams compare, and survives a helper's restart. It implements
// traffic.Source.
func (d *Daemon) Counters() traffic.Reading {
	r := traffic.Reading{Peers: map[int64]traffic.Counter{}, Upstreams: map[string]traffic.Counter{}}
	d.mu.Lock()
	if d.down != nil {
		if peers, err := d.down.Peers(); err == nil {
			for _, p := range peers {
				if dp, ok := d.devicePeers[p.PublicKey]; ok {
					// The device received what the client sent.
					r.Peers[dp.ID] = traffic.Counter{Down: p.TxBytes, Up: p.RxBytes}
				}
			}
		}
	}
	ifaces := map[string]string{}
	for _, u := range d.ups {
		ifaces[u.Name] = u.InterfaceName()
	}
	d.mu.Unlock()

	if d.interfaceCounters == nil {
		return r
	}
	for name, iface := range ifaces {
		// An interface missing for now is left out; its upstream keeps its
		// total in the sampler.
		if rx, tx, err := d.interfaceCounters(iface); err == nil {
			// The kernel transmits into the upstream's TUN what the clients
			// send.
			r.Upstreams[name] = traffic.Counter{Down: rx, Up: tx}
		}
	}
	return r
}

// Traffic is the latest sample of every peer's and upstream's bytes and
// rates; empty before the daemon runs.
func (d *Daemon) Traffic() traffic.Snapshot { return d.sampler.Latest() }

// sampleTraffic samples the counters every TrafficInterval until the
// returned function stops it.
func (d *Daemon) sampleTraffic(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	d.sampler.Source = d
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.sampler.Run(ctx, TrafficInterval)
	}()
	return func() {
		cancel()
		<-done
	}
}
