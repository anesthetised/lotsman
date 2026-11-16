package proxy

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.getoutline.org/sdk/transport"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Keepalive on the client side of a TCP flow, so a flow whose client went
// away without a FIN or RST is reclaimed instead of held forever.
const (
	keepaliveIdle     = time.Minute
	keepaliveInterval = 30 * time.Second
	keepaliveCount    = 4
)

// target is where the client addressed a flow: the local end of the
// stack's endpoint, since the stack accepts every destination.
func target(id stack.TransportEndpointID) netip.AddrPort {
	addr, _ := netip.AddrFromSlice(id.LocalAddress.AsSlice())
	return netip.AddrPortFrom(addr, id.LocalPort)
}

// flowContext lives as long as one flow. A dialer may keep using the context
// it dialed with after the dial returns (one that returns before reaching
// the server does), so the dial timeout must not cancel it once the dial
// has returned in time. timedOut reports whether the dial took too long.
func (s *Stack) flowContext() (ctx context.Context, cancel context.CancelFunc, dialed func() (timedOut bool)) {
	ctx, cancel = context.WithCancel(s.ctx)
	timer := time.AfterFunc(s.opts.DialTimeout, cancel)
	return ctx, cancel, func() bool { return !timer.Stop() }
}

// handleTCP runs in its own goroutine for every new TCP flow. It dials the
// server first and completes the client's handshake only on success, so an
// unreachable server looks like a refused connection, not a hung one.
func (s *Stack) handleTCP(r *tcp.ForwarderRequest) {
	if !s.begin() {
		r.Complete(true)
		return
	}
	defer s.end()
	dst := target(r.ID())
	ctx, cancel, dialed := s.flowContext()
	defer cancel()
	remote, err := s.dialers.Load().Streams.DialStream(ctx, dst.String())
	if dialed() && err == nil {
		remote.Close()
		err = context.DeadlineExceeded
	}
	if err != nil {
		s.log.Debug("tcp dial", "target", dst, "err", err)
		r.Complete(true)
		return
	}
	if !s.hold(remote) {
		r.Complete(true)
		return
	}
	defer s.release(remote)
	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	r.Complete(false)
	if tcpErr != nil {
		s.log.Debug("tcp accept", "target", dst, "err", tcpErr)
		return
	}
	setKeepalive(ep)
	local := gonet.NewTCPConn(&wq, ep)
	if !s.hold(local) {
		return
	}
	defer s.release(local)
	relayTCP(local, ep.Abort, remote)
}

func setKeepalive(ep tcpip.Endpoint) {
	ep.SocketOptions().SetKeepAlive(true)
	idle := tcpip.KeepaliveIdleOption(keepaliveIdle)
	interval := tcpip.KeepaliveIntervalOption(keepaliveInterval)
	ep.SetSockOpt(&idle)
	ep.SetSockOpt(&interval)
	ep.SetSockOptInt(tcpip.KeepaliveCountOption, keepaliveCount)
}

// relayTCP copies both ways until both sides are done. An end of stream is
// passed on as a half-close, so the other direction keeps flowing. An error
// in either direction aborts the flow, and the client gets a reset (abort
// sends it), not a clean end, so a failure looks like one.
func relayTCP(local *gonet.TCPConn, abort func(), remote transport.StreamConn) {
	fail := sync.OnceFunc(func() {
		abort()
		local.Close()
		remote.Close()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := io.Copy(remote, local); err != nil {
			fail()
			return
		}
		remote.CloseWrite()
	}()
	if _, err := io.Copy(local, remote); err != nil {
		fail()
	} else {
		local.CloseWrite()
	}
	<-done
}

// handleUDP runs on the stack's receive path for the first packet of every
// new UDP flow (a new client address and port to a new destination), so it
// must not block. A refused packet is dropped.
func (s *Stack) handleUDP(r *udp.ForwarderRequest) {
	if !s.begin() {
		return
	}
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		s.end()
		s.log.Debug("udp accept", "err", err)
		return
	}
	dst := target(r.ID())
	local := gonet.NewUDPConn(&wq, ep)
	go func() {
		defer s.end()
		s.relayUDP(local, dst)
	}()
}

// relayUDP carries one UDP flow until it is idle for UDPIdleTimeout, or
// DNSIdleTimeout for DNS. Replies must come from the flow's destination: the
// stack can answer the client only from that address.
func (s *Stack) relayUDP(local *gonet.UDPConn, dst netip.AddrPort) {
	if !s.hold(local) {
		return
	}
	defer s.release(local)
	timeout := s.opts.UDPIdleTimeout
	if dst.Port() == 53 {
		timeout = s.opts.DNSIdleTimeout
	}
	// Like a TCP dial, the listener may keep its context for the flow's life.
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	remote, err := s.dialers.Load().Packets.ListenPacket(ctx)
	if err != nil {
		s.log.Debug("udp listen", "target", dst, "err", err)
		return
	}
	if !s.hold(remote) {
		return
	}
	defer s.release(remote)

	idle := time.AfterFunc(timeout, func() {
		local.Close()
		remote.Close()
	})
	defer idle.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, maxSegment)
		for {
			n, from, err := remote.ReadFrom(buf)
			if err != nil {
				local.Close()
				return
			}
			if !fromTarget(from, dst) {
				continue
			}
			idle.Reset(timeout)
			if _, err := local.Write(buf[:n]); err != nil {
				return
			}
		}
	}()
	to := net.UDPAddrFromAddrPort(dst)
	buf := make([]byte, maxSegment)
	for {
		n, err := local.Read(buf)
		if err != nil {
			break
		}
		idle.Reset(timeout)
		if _, err := remote.WriteTo(buf[:n], to); err != nil {
			s.log.Debug("udp write", "target", dst, "err", err)
		}
	}
	remote.Close()
	<-done
}

// fromTarget reports whether a reply came from dst. The Shadowsocks packet
// listener reports IPv4 sources in their IPv4-mapped form.
func fromTarget(from net.Addr, dst netip.AddrPort) bool {
	addr, ok := from.(*net.UDPAddr)
	if !ok {
		return false
	}
	ap := addr.AddrPort()
	return ap.Addr().Unmap() == dst.Addr() && ap.Port() == dst.Port()
}
