package tunnel

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/amnezia-vpn/amneziawg-go/conn"
)

// addressBind is deliberately a single socket without AWG's batching/offload
// machinery. Its local address and family are fixed for its entire lifetime.
type addressBind struct {
	mu     sync.Mutex
	addr   netip.Addr
	socket *net.UDPConn
}

func newAddressBind(addr netip.Addr) conn.Bind { return &addressBind{addr: addr.Unmap()} }

func (b *addressBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.socket != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	if !b.addr.IsValid() {
		return nil, 0, fmt.Errorf("listen address is invalid")
	}
	network := "udp6"
	if b.addr.Is4() {
		network = "udp4"
	}
	socket, err := net.ListenUDP(network, net.UDPAddrFromAddrPort(netip.AddrPortFrom(b.addr, port)))
	if err != nil {
		return nil, 0, err
	}
	b.socket = socket
	// Capture this socket so a receiver from an earlier Open remains closed
	// after reopening; it must never start reading the new socket.
	receive := func(bufs [][]byte, sizes []int, endpoints []conn.Endpoint) (int, error) {
		n, from, err := socket.ReadFromUDPAddrPort(bufs[0])
		if err != nil {
			return 0, err
		}
		sizes[0] = n
		endpoints[0] = &conn.StdNetEndpoint{AddrPort: from}
		return 1, nil
	}
	return []conn.ReceiveFunc{receive}, socket.LocalAddr().(*net.UDPAddr).AddrPort().Port(), nil
}

func (b *addressBind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.socket == nil {
		return nil
	}
	err := b.socket.Close()
	b.socket = nil
	return err
}

func (b *addressBind) Send(bufs [][]byte, endpoint conn.Endpoint) error {
	b.mu.Lock()
	socket := b.socket
	b.mu.Unlock()
	if socket == nil {
		return net.ErrClosed
	}
	ep, ok := endpoint.(*conn.StdNetEndpoint)
	if !ok || ep == nil {
		return conn.ErrWrongEndpointType
	}
	// The device batches according to both TUN and bind sizes; handshake
	// initiation also sends junk and signature packets together.
	for _, buf := range bufs {
		if _, err := socket.WriteToUDPAddrPort(buf, ep.AddrPort); err != nil {
			return err
		}
	}
	return nil
}

func (*addressBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	addr, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return &conn.StdNetEndpoint{AddrPort: netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())}, nil
}

func (*addressBind) BatchSize() int { return 1 }

func (*addressBind) SetMark(mark uint32) error {
	if mark != 0 {
		return fmt.Errorf("address-bound socket marks: %w", errors.ErrUnsupported)
	}
	return nil
}
