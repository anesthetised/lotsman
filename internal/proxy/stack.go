// Package proxy turns a TUN into an upstream through a TCP/UDP proxy such as
// a Shadowsocks (Outline) server: a userspace TCP/IP stack (gVisor netstack)
// terminates the TCP and UDP flows the kernel routes into the TUN and
// re-originates each one through the proxy. The stack does not know the
// proxy protocol; a Key builds the Dialers for one. Every Stack is
// independent, so one process can run several.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"golang.getoutline.org/sdk/transport"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

const (
	nicID = 1
	// tunOffset is the headroom the TUN may use in front of each packet,
	// such as for a virtio-net header on Linux; amneziawg-go uses the same.
	tunOffset = 16
	// maxSegment bounds a coalesced write to the TUN (Linux GRO).
	maxSegment = 65535
	// queueSize is the number of packets the stack may queue toward the TUN.
	queueSize = 1024
	// maxPendingTCP bounds TCP handshakes waiting for their proxy dial.
	maxPendingTCP = 1024
)

// DefaultMaxFlows is the default Options.MaxFlows: enough for dozens of busy
// clients, while an idle TCP flow costs tens of KiB.
const DefaultMaxFlows = 4096

// Options tune a Stack; zero values take the defaults.
type Options struct {
	// DialTimeout bounds connecting a new TCP flow to the server. The
	// client's handshake completes only after the dial succeeds; on failure
	// it gets a reset. Default 10 s.
	DialTimeout time.Duration
	// UDPIdleTimeout closes a UDP flow that saw no packet in either
	// direction. Default 1 min.
	UDPIdleTimeout time.Duration
	// DNSIdleTimeout replaces UDPIdleTimeout for UDP flows to port 53,
	// which rarely carry more than one query. Default 10 s.
	DNSIdleTimeout time.Duration
	// MaxFlows caps the TCP and UDP flows open at once, TCP flows still
	// dialing included. A TCP flow beyond it is reset; a UDP packet that
	// would start one is dropped. Default DefaultMaxFlows.
	MaxFlows int
}

// Dialers carry a stack's flows to the proxy server. A dialer may return
// before it reaches the server and keep using the context it got for the
// life of the flow; the stack gives every flow its own.
type Dialers struct {
	Streams transport.StreamDialer   // TCP
	Packets transport.PacketListener // UDP
	// Closer, if set, releases what the dialers hold, such as a client
	// instance of the protocol. The stack closes it when it closes or when
	// SetDialers replaces it; flows still using it may end then.
	Closer io.Closer
}

func (d *Dialers) close() {
	if d.Closer != nil {
		d.Closer.Close()
	}
}

// Stats is a snapshot of a stack's flow counters.
type Stats struct {
	Flows    int    // open now
	MaxFlows int    // the current limit
	Refused  uint64 // flows refused at the limit since the stack started
}

// Stack carries the TCP and UDP flows that arrive on one TUN through one
// proxy server.
type Stack struct {
	tun     tun.Device
	link    *channel.Endpoint
	net     *stack.Stack
	dialers atomic.Pointer[Dialers]
	opts    Options
	log     *slog.Logger
	ctx     context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex
	closed   bool
	flows    int
	maxFlows int
	refused  uint64
	conns    map[io.Closer]struct{} // open on either side of a flow, closed by Close
	wg       sync.WaitGroup         // the TUN loops and every flow
	failed   chan error             // see Failed
}

// New starts a stack on t and carries its flows through d. It owns both from
// now on, and closes them if it fails.
func New(t tun.Device, d Dialers, opts Options, log *slog.Logger) (s *Stack, err error) {
	defer func() {
		if err != nil {
			t.Close()
			d.close()
		}
	}()
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 10 * time.Second
	}
	if opts.UDPIdleTimeout == 0 {
		opts.UDPIdleTimeout = time.Minute
	}
	if opts.DNSIdleTimeout == 0 {
		opts.DNSIdleTimeout = 10 * time.Second
	}
	if opts.MaxFlows == 0 {
		opts.MaxFlows = DefaultMaxFlows
	}
	mtu, err := t.MTU()
	if err != nil {
		return nil, fmt.Errorf("tun mtu: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s = &Stack{
		tun:      t,
		link:     channel.New(queueSize, uint32(mtu), ""),
		opts:     opts,
		log:      log,
		ctx:      ctx,
		cancel:   cancel,
		maxFlows: opts.MaxFlows,
		conns:    map[io.Closer]struct{}{},
		failed:   make(chan error, 1),
	}
	s.dialers.Store(&d)
	s.net = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if err = s.configure(); err != nil {
		s.net.Close()
		cancel()
		return nil, err
	}
	s.wg.Add(2)
	go s.readTUN()
	go s.writeTUN()
	return s, nil
}

func (s *Stack) configure() error {
	sack := tcpip.TCPSACKEnabled(true)
	moderate := tcpip.TCPModerateReceiveBufferOption(true)
	for _, opt := range []tcpip.SettableTransportProtocolOption{&sack, &moderate} {
		if err := s.net.SetTransportProtocolOption(tcp.ProtocolNumber, opt); err != nil {
			return fmt.Errorf("tcp option %T: %s", opt, err)
		}
	}
	if err := s.net.CreateNIC(nicID, s.link); err != nil {
		return fmt.Errorf("create nic: %s", err)
	}
	// Accept packets to any destination and answer from it: every flow is
	// addressed to somewhere on the internet, not to the stack.
	if err := s.net.SetPromiscuousMode(nicID, true); err != nil {
		return fmt.Errorf("promiscuous mode: %s", err)
	}
	if err := s.net.SetSpoofing(nicID, true); err != nil {
		return fmt.Errorf("spoofing: %s", err)
	}
	s.net.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	tcpForwarder := tcp.NewForwarder(s.net, 0, maxPendingTCP, s.handleTCP)
	s.net.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)
	udpForwarder := udp.NewForwarder(s.net, s.handleUDP)
	s.net.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)
	return nil
}

// SetDialers makes new flows use d, such as after the server moved to
// another address, and closes the dialers it replaces. Open flows keep their
// connections unless closing their dialers ends them. Once the stack is
// closed, d is closed at once.
func (s *Stack) SetDialers(d Dialers) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		d.close()
		return
	}
	s.dialers.Swap(&d).close()
}

// Failed delivers the error that stopped the stack carrying traffic: reading
// the TUN failed. It is closed without a value once the stack stops for
// another reason, such as Close.
func (s *Stack) Failed() <-chan error { return s.failed }

// SetMaxFlows changes Options.MaxFlows, 0 meaning the default. Flows
// already open stay, even beyond a lower limit.
func (s *Stack) SetMaxFlows(n int) {
	if n == 0 {
		n = DefaultMaxFlows
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxFlows = n
}

func (s *Stack) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Flows: s.flows, MaxFlows: s.maxFlows, Refused: s.refused}
}

// Close stops the stack, aborts every flow, closes the TUN and returns once
// every goroutine the stack started has exited.
func (s *Stack) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()

	s.cancel()           // stops writeTUN and pending dials
	err := s.tun.Close() // stops readTUN, so no new flows start
	for c := range conns {
		c.Close()
	}
	// A flow whose client went away during the handshake waits minutes for
	// the last ACK, and only aborting its endpoint ends that. Abort until
	// every flow is gone: one that was creating its endpoint during a sweep
	// is caught by the next.
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	for aborted := false; !aborted; {
		for _, ep := range s.net.RegisteredEndpoints() {
			ep.Abort()
		}
		select {
		case <-done:
			aborted = true
		case <-time.After(50 * time.Millisecond):
		}
	}
	s.net.Close()
	s.net.Wait()
	s.dialers.Load().close()
	return err
}

// begin admits a new flow and counts it toward Close's wait. It reports
// false once the stack is closing or already carries its maximum of flows.
func (s *Stack) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if s.flows >= s.maxFlows {
		s.refused++
		return false
	}
	s.flows++
	s.wg.Add(1)
	return true
}

// end finishes a flow that begin admitted.
func (s *Stack) end() {
	s.mu.Lock()
	s.flows--
	s.mu.Unlock()
	s.wg.Done()
}

// hold registers c for Close to abort. Once the stack is closing it closes
// c instead and reports false.
func (s *Stack) hold(c io.Closer) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

// release closes c and forgets it.
func (s *Stack) release(c io.Closer) {
	c.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

// readTUN feeds packets from the TUN into the stack.
func (s *Stack) readTUN() {
	defer s.wg.Done()
	defer close(s.failed)
	mtu := int(s.link.MTU())
	bufs := make([][]byte, s.tun.BatchSize())
	sizes := make([]int, len(bufs))
	for i := range bufs {
		bufs[i] = make([]byte, tunOffset+mtu)
	}
	for {
		n, err := s.tun.Read(bufs, sizes, tunOffset)
		for i := range n {
			s.inject(bufs[i][tunOffset : tunOffset+sizes[i]])
		}
		switch {
		case err == nil, errors.Is(err, tun.ErrTooManySegments):
		case s.ctx.Err() != nil || errors.Is(err, os.ErrClosed):
			return
		default:
			s.log.Error("read tun, the upstream stops", "err", err)
			s.failed <- err
			return
		}
	}
}

// inject hands an IPv4 packet to the stack; the tunnel carries nothing else.
// ICMP goes in too, as errors about the stack's own flows. The stack does
// not answer pings to the addresses it accepts in promiscuous mode, so it
// does not fake reachability.
func (s *Stack) inject(packet []byte) {
	if len(packet) < header.IPv4MinimumSize || header.IPVersion(packet) != header.IPv4Version {
		return
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
	s.link.InjectInbound(header.IPv4ProtocolNumber, pkt)
	pkt.DecRef()
}

// writeTUN writes the stack's packets to the TUN, in batches so Linux can
// coalesce them.
func (s *Stack) writeTUN() {
	defer s.wg.Done()
	bufs := make([][]byte, s.tun.BatchSize())
	for i := range bufs {
		bufs[i] = make([]byte, 0, tunOffset+maxSegment)
	}
	batch := make([][]byte, 0, len(bufs))
	for {
		pkt := s.link.ReadContext(s.ctx)
		if pkt == nil {
			return
		}
		batch = batch[:0]
		for pkt != nil {
			b := bufs[len(batch)][:tunOffset]
			for _, v := range pkt.AsSlices() {
				b = append(b, v...)
			}
			pkt.DecRef()
			batch = append(batch, b)
			if len(batch) == len(bufs) {
				break
			}
			pkt = s.link.Read()
		}
		if _, err := s.tun.Write(batch, tunOffset); err != nil {
			if s.ctx.Err() != nil || errors.Is(err, os.ErrClosed) {
				return
			}
			s.log.Debug("write tun", "err", err)
		}
	}
}
