package tunnel

import (
	"runtime"
	"slices"

	"github.com/amnezia-vpn/amneziawg-go/v3/conn"
)

func newUpstreamBind(disableUDPGSO bool) conn.Bind {
	bind := conn.NewDefaultBind()
	if disableUDPGSO && runtime.GOOS == "linux" {
		return unsegmentedBind{bind}
	}
	return bind
}

type unsegmentedBind struct{ conn.Bind }

func (b unsegmentedBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	receivers, port, err := b.Bind.Open(port)
	if err != nil {
		return nil, 0, err
	}
	for i, receive := range receivers {
		receivers[i] = func(bufs [][]byte, sizes []int, endpoints []conn.Endpoint) (int, error) {
			n, err := receive(bufs, sizes, endpoints)
			if err == nil {
				// AWG starts its route listener only for the concrete
				// StdNetBind. Let the kernel choose the source on every send
				// instead of retaining an interface that may become stale.
				for j, size := range sizes[:n] {
					if size > 0 {
						endpoints[j].ClearSrc()
					}
				}
			}
			return n, err
		}
	}
	return receivers, port, nil
}

func (b unsegmentedBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	if len(bufs) <= 1 {
		return b.Bind.Send(bufs, ep)
	}
	// The pinned StdNetBind only coalesces a packet when it fits into
	// the spare capacity of the first buffer. Clip copies of the slice
	// headers so sendmmsg still batches the packets as separate datagrams.
	batch := make([][]byte, len(bufs))
	for len(bufs) > 0 {
		n := 0
		for n < len(bufs) && len(bufs[n]) > 0 {
			batch[n] = slices.Clip(bufs[n])
			n++
		}
		if n == 0 {
			// A zero-length junk packet would still fit in a clipped
			// buffer. Give it its own send to preserve the datagram.
			batch[0] = bufs[0]
			n = 1
		}
		if err := b.Bind.Send(batch[:n], ep); err != nil {
			return err
		}
		bufs = bufs[n:]
	}
	return nil
}
