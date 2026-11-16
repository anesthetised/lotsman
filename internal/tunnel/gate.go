package tunnel

import (
	"os"
	"sync"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
)

// configGate delays the runtime's first TUN read until the device is configured.
//
// amneziawg-go's RoutineReadFromTUN loads the S4 transport padding at the top of
// its loop and only then blocks in tun.Read (device/send.go, "padding :=
// device.paddings.transport.Load()" and the Read below it). NewDevice starts that
// goroutine at once, and we apply the config afterwards, so the loop is already
// parked in Read with padding 0 when S4 is set: the first packet it reads goes
// out unpadded and the peer drops it. The gate holds that first Read back, then
// returns (0, nil); the loop treats an empty read as a no-op and iterates, so
// the padding is reloaded before the real Read. Later reads pass straight through.
type configGate struct {
	tun.Device

	configured chan struct{} // closed after the first successful Configure
	closed     chan struct{} // closed by Close
	cfgOnce    sync.Once
	closeOnce  sync.Once
	passed     bool // touched only by the runtime's single TUN reader goroutine
}

func newConfigGate(t tun.Device) *configGate {
	return &configGate{Device: t, configured: make(chan struct{}), closed: make(chan struct{})}
}

// markConfigured releases the first Read; call it after a successful Configure.
func (g *configGate) markConfigured() { g.cfgOnce.Do(func() { close(g.configured) }) }

func (g *configGate) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	if !g.passed {
		g.passed = true
		select {
		case <-g.configured:
			return 0, nil
		case <-g.closed:
			return 0, os.ErrClosed
		}
	}
	return g.Device.Read(bufs, sizes, offset)
}

// Close unblocks a gated Read before closing the TUN, so closing a device that
// was never configured does not wait on the gate.
func (g *configGate) Close() error {
	g.closeOnce.Do(func() { close(g.closed) })
	return g.Device.Close()
}
