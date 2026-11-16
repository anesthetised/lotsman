package proxy

import (
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"

	"github.com/anesthetised/lotsman/internal/proxy/proxytest"
)

// packetTUN is a TUN whose reads return injected packets and whose writes
// are counted and discarded, so a test controls exactly what the stack sees.
type packetTUN struct {
	in      chan []byte
	closed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	written int
}

func newPacketTUN() *packetTUN {
	return &packetTUN{in: make(chan []byte, 16), closed: make(chan struct{})}
}

func (t *packetTUN) File() *os.File           { return nil }
func (t *packetTUN) MTU() (int, error)        { return 1500, nil }
func (t *packetTUN) Name() (string, error)    { return "packet", nil }
func (t *packetTUN) Events() <-chan tun.Event { return nil }
func (t *packetTUN) BatchSize() int           { return 1 }

func (t *packetTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case p := <-t.in:
		sizes[0] = copy(bufs[0][offset:], p)
		return 1, nil
	case <-t.closed:
		return 0, os.ErrClosed
	}
}

func (t *packetTUN) Write(bufs [][]byte, _ int) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.written += len(bufs)
	return len(bufs), nil
}

func (t *packetTUN) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

// syn is the first packet of a TCP connection from src to dst.
func syn(src, dst netip.AddrPort) []byte {
	pkt := make([]byte, header.IPv4MinimumSize+header.TCPMinimumSize)
	from, to := tcpip.AddrFrom4(src.Addr().As4()), tcpip.AddrFrom4(dst.Addr().As4())
	ip := header.IPv4(pkt)
	ip.Encode(&header.IPv4Fields{TotalLength: uint16(len(pkt)), TTL: 64, Protocol: uint8(header.TCPProtocolNumber), SrcAddr: from, DstAddr: to})
	ip.SetChecksum(^ip.CalculateChecksum())
	tcp := header.TCP(pkt[header.IPv4MinimumSize:])
	tcp.Encode(&header.TCPFields{SrcPort: src.Port(), DstPort: dst.Port(), SeqNum: 1, DataOffset: header.TCPMinimumSize,
		Flags: header.TCPFlagSyn, WindowSize: 65535})
	tcp.SetChecksum(^tcp.CalculateChecksum(header.PseudoHeaderChecksum(header.TCPProtocolNumber, from, to, uint16(len(tcp)))))
	return pkt
}

// A client that sends a SYN and goes away leaves the stack waiting for the
// last ACK of the handshake, for minutes of SYN-ACK retransmissions. Close
// must not wait for that.
func TestCloseAbortsPendingHandshakes(t *testing.T) {
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxytest.Serve(t, echo, proxytest.Echo)
	server := proxytest.NewServer(t, "secret", map[string]string{echoTarget.String(): echo.Addr().String()})
	dev := newPacketTUN()
	s, err := New(dev, dialers(t, server), Options{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	dev.in <- syn(netip.MustParseAddrPort("192.0.0.8:40000"), echoTarget)
	// The server was dialed and the SYN-ACK went out: the stack now waits
	// for an ACK that never comes.
	waitFor(t, "the SYN-ACK", func() bool {
		dev.mu.Lock()
		defer dev.mu.Unlock()
		return dev.written > 0 && s.Stats().Flows == 1
	})

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close waits for a handshake that cannot complete")
	}
}
